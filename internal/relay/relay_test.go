package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellymesh/internal/store"
)

const reference = "0123456789abcdef0123456789abcdef"

type references map[string]store.Materialized

func (refs references) ByReference(_ context.Context, reference string) (store.Materialized, bool, error) {
	item, ok := refs[reference]
	return item, ok, nil
}

type policy struct{ refuse bool }

func (p *policy) MayPlay(context.Context, string, string) error {
	if p.refuse {
		return ErrRefused
	}
	return nil
}

// upstream serves a response built by respond and records what it saw.
type upstream struct {
	respond   func(ctx context.Context, method string, header http.Header) (*http.Response, error)
	calls     atomic.Int32
	cancelled atomic.Bool
}

func (source *upstream) Media(ctx context.Context, _ string, method string, _ string, header http.Header) (*http.Response, error) {
	source.calls.Add(1)
	go func() {
		<-ctx.Done()
		source.cancelled.Store(true)
	}()
	return source.respond(ctx, method, header)
}

func newRelay(t *testing.T, source *upstream, refuse bool) *Server {
	t.Helper()
	allowed, err := ParseAllowed("127.0.0.0/8, ::1")
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	refs := references{reference: {SourceNodeID: "cedar", ItemID: "movie-1", LibraryID: "lib-movies", Reference: reference}}
	return New(allowed, refs, &policy{refuse: refuse}, source, nil)
}

func fixed(status int, body string, headers map[string]string) func(context.Context, string, http.Header) (*http.Response, error) {
	return func(context.Context, string, http.Header) (*http.Response, error) {
		response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		for name, value := range headers {
			response.Header.Set(name, value)
		}
		return response, nil
	}
}

func call(server *Server, method string, path string, remote string, rangeHeader string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remote
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// C-PR-5: only allow-listed client addresses may use the relay, whatever
// reference they present.
func TestOnlyAllowedClientsMayUseTheRelay(t *testing.T) {
	source := &upstream{respond: fixed(http.StatusOK, "media", nil)}
	server := newRelay(t, source, false)
	if code := call(server, http.MethodGet, Path+reference, "192.0.2.10:5000", "").Code; code != http.StatusForbidden {
		t.Fatalf("a client outside the allow-list: status %d, want 403", code)
	}
	if code := call(server, http.MethodGet, Path+reference, "[::1]:5000", "").Code; code != http.StatusOK {
		t.Fatalf("an allowed IPv6 loopback client: status %d, want 200", code)
	}
	if source.calls.Load() != 1 {
		t.Fatal("a refused client must not reach the source")
	}
}

// C-MA-4, C-PR-5: a reference that was never issued, or was revoked, or is
// malformed, finds nothing, and a refusal by policy reaches no source.
func TestUnknownReferencesAndPolicyRefusalsReachNoSource(t *testing.T) {
	source := &upstream{respond: fixed(http.StatusOK, "media", nil)}
	server := newRelay(t, source, false)
	for _, path := range []string{Path + "ffffffffffffffffffffffffffffffff", Path + "not-a-reference", Path + "../etc/passwd"} {
		if code := call(server, http.MethodGet, path, "127.0.0.1:5000", "").Code; code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
	refusing := newRelay(t, source, true)
	if code := call(refusing, http.MethodGet, Path+reference, "127.0.0.1:5000", "").Code; code != http.StatusNotFound {
		t.Fatalf("an item the policy refuses: status %d, want 404", code)
	}
	if source.calls.Load() != 0 {
		t.Fatal("no refused request may reach a source")
	}
}

// C-PB-1: ranges, their response headers, and HEAD pass through unchanged.
func TestRangesAndHeadPassThrough(t *testing.T) {
	var sawRange string
	source := &upstream{respond: func(_ context.Context, method string, header http.Header) (*http.Response, error) {
		sawRange = header.Get("Range")
		body := "bytes-100-to-199"
		if method == http.MethodHead {
			body = ""
		}
		return fixed(http.StatusPartialContent, body, map[string]string{
			"Content-Range": "bytes 100-199/5000", "Content-Length": "100", "Content-Type": "video/x-matroska",
			"Accept-Ranges": "bytes", "Set-Cookie": "leak=1",
		})(nil, method, header)
	}}
	server := newRelay(t, source, false)
	recorder := call(server, http.MethodGet, Path+reference, "127.0.0.1:5000", "bytes=100-199")
	if recorder.Code != http.StatusPartialContent || sawRange != "bytes=100-199" || recorder.Body.String() != "bytes-100-to-199" {
		t.Fatalf("range: status %d, forwarded range %q, body %q", recorder.Code, sawRange, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Range") != "bytes 100-199/5000" || recorder.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("range headers: %v", recorder.Header())
	}
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatal("only the listed headers may pass through")
	}
	if head := call(server, http.MethodHead, Path+reference, "127.0.0.1:5000", ""); head.Code != http.StatusPartialContent || head.Body.Len() != 0 {
		t.Fatalf("head: status %d, body %d bytes", head.Code, head.Body.Len())
	}
}

// C-PB-2: an unreachable source fails cleanly, as a 502 with a reason.
func TestAnUnreachableSourceFailsCleanly(t *testing.T) {
	source := &upstream{respond: func(context.Context, string, http.Header) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	recorder := call(newRelay(t, source, false), http.MethodGet, Path+reference, "127.0.0.1:5000", "")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "not reachable") {
		t.Fatalf("status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "connection refused") {
		t.Fatal("the relay's answer must not expose the transport error")
	}
}

// C-PB-1: the relay streams as the source sends, never waiting for the whole
// file, and a client that leaves cancels the request to the source.
func TestTheRelayStreamsAndCancelsUpstream(t *testing.T) {
	reader, writer := io.Pipe()
	source := &upstream{respond: func(ctx context.Context, _ string, _ http.Header) (*http.Response, error) {
		go func() {
			<-ctx.Done()
			writer.CloseWithError(ctx.Err())
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/x-matroska"}}, Body: reader}, nil
	}}
	server := httptest.NewServer(newRelay(t, source, false).Handler())
	defer server.Close()

	go writer.Write([]byte("first chunk"))
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+Path+reference, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	first := make([]byte, len("first chunk"))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(response.Body, first)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil || string(first) != "first chunk" {
			t.Fatalf("first chunk: %q, %v", first, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the relay held the first chunk back while the source was still sending")
	}

	cancel()
	response.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !source.cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !source.cancelled.Load() {
		t.Fatal("a client that leaves must cancel the request to the source")
	}
}

func TestParseAllowed(t *testing.T) {
	networks, err := ParseAllowed("127.0.0.1, 172.17.0.0/16 ,::1")
	if err != nil || len(networks) != 3 {
		t.Fatalf("parse: %v, %v", networks, err)
	}
	for _, bad := range []string{"", " , ", "not-an-address", "10.0.0.0/99"} {
		if _, err := ParseAllowed(bad); err == nil {
			t.Errorf("ParseAllowed(%q) should fail", bad)
		}
	}
}
