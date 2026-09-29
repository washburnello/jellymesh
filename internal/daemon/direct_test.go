package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/config"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/jellyfin/jellyfintest"
	"jellymesh/internal/natpath"
	"jellymesh/internal/store"
)

// failingReader yields data, then fails as a dropped direct path would.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func (f *failingReader) Close() error { return nil }

// C-NT-6: a direct stream that breaks part-way continues over TCP from the
// exact byte where it stopped, for a whole file and for a range, and a TCP
// answer that does not resume there is not spliced in.
func TestADirectStreamThatBreaksContinuesOverTCP(t *testing.T) {
	file := bytes.Repeat([]byte("0123456789"), 10_000)
	cases := []struct {
		name       string
		status     int
		start, end int64
	}{
		{"whole file", http.StatusOK, 0, int64(len(file)) - 1},
		{"range", http.StatusPartialContent, 20_000, 59_999},
	}
	for _, c := range cases {
		direct := &http.Response{StatusCode: c.status, Header: http.Header{}, ContentLength: c.end - c.start + 1,
			Body: &failingReader{data: file[c.start : c.start+7_000], err: errors.New("stream reset")}}
		if c.status == http.StatusPartialContent {
			direct.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", c.start, c.end, len(file)))
		}
		var failures, reopenedFrom int64 = 0, -1
		body := resumeOverTCP(context.Background(), direct, func(from int64, end int64) (io.ReadCloser, error) {
			reopenedFrom = from
			if end != c.end {
				t.Fatalf("%s: resumed to %d, want %d", c.name, end, c.end)
			}
			return io.NopCloser(bytes.NewReader(file[from : end+1])), nil
		}, func(error) { failures++ })
		got, err := io.ReadAll(body)
		if err != nil || !bytes.Equal(got, file[c.start:c.end+1]) {
			t.Fatalf("%s: %d of %d bytes, %v", c.name, len(got), c.end-c.start+1, err)
		}
		if reopenedFrom != c.start+7_000 || failures != 1 {
			t.Fatalf("%s: resumed from %d after %d failures", c.name, reopenedFrom, failures)
		}
	}

	// The TCP answer must start where the direct one stopped.
	open := func(header http.Header) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Range": {"bytes 0-99/100"}},
			Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	if _, err := reopenFrom(open, nil, 50, 99); err == nil {
		t.Fatal("an answer starting elsewhere must not be spliced in")
	}
}

func withDirectPaths(configure func(*config.Config)) func(*config.Config) {
	return func(cfg *config.Config) {
		if configure != nil {
			configure(cfg)
		}
		cfg.DirectListenAddress = "127.0.0.1:0"
		cfg.DirectOfferLocal = true
	}
}

func (d *testDaemon) waitForPath(t *testing.T, peerID string, want string) DirectStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status := d.status().Direct
		for _, peer := range status.Peers {
			if peer.NodeID == peerID && peer.Path == want {
				return status
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached a %s path to %s: %+v", d.name, want, peerID, status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// C-NT-5, C-NT-6: once a direct path is open, media goes over it with every
// relay guarantee intact (whole files, ranges, HEAD, the head cache, and the
// source's policy on each request), and when it drops, media carries on
// over TCP, the node reports the change, and the path is reopened later.
// directPair is two daemons with direct paths, cedar publishing a film that
// walnut has materialized, and walnut's relay.
func directPair(t *testing.T, cedarConfig func(*config.Config), walnutConfig func(*config.Config)) (cedar *testDaemon, walnut *testDaemon, relayURL string, strmURL string, media []byte) {
	t.Helper()
	cedarJellyfin, walnutJellyfin := jellyfintest.New(), jellyfintest.New()
	t.Cleanup(cedarJellyfin.Close)
	t.Cleanup(walnutJellyfin.Close)
	cedarJellyfin.AddLibrary("lib-movies", "Movies", "movies")
	cedarJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-movies")
	cedarJellyfin.AddItem("lib-movies", jellyfin.Item{ID: "movie-1", Name: "Probe Film", ProductionYear: 2001, Type: "Movie",
		Path: "/media/movies/probe.mkv", ProviderIDs: map[string]string{"Tmdb": "603"}})
	media = bytes.Repeat([]byte("frame"), 300_000)
	cedarJellyfin.SetMedia("movie-1", media)
	walnutJellyfin.AddLibrary("lib-docs", "Documentaries", "movies")
	walnutJellyfin.AddUser("jellymesh", "service-user-password-for-tests", false, "lib-docs")
	walnutJellyfin.AddItem("lib-docs", jellyfin.Item{ID: "doc-1", Name: "Oceans", Type: "Movie", Path: "/media/docs/oceans.mkv"})

	cedar = startDaemonWith(t, "cedar", t.TempDir(), "127.0.0.1:0", withDirectPaths(combine(withServiceUser(cedarJellyfin), cedarConfig)))
	walnutDir := t.TempDir()
	walnut = startDaemonWith(t, "walnut", walnutDir, "127.0.0.1:0", withDirectPaths(combine(withServiceUser(walnutJellyfin), walnutConfig)))
	// These tests re-offer quickly on purpose; natpath tests the limit.
	cedar.node.direct.offerInterval, walnut.node.direct.offerInterval = time.Millisecond, time.Millisecond
	cedar.must(http.MethodPost, "/admin/v1/group", FoundRequest{GroupID: "group-1"}, nil)
	cedar.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-movies", Roots: []string{"/media/movies"}}, nil)
	walnut.must(http.MethodPost, "/admin/v1/publications", PublishRequest{LibraryID: "lib-docs", Roots: []string{"/media/docs"}}, nil)
	joinOffering(t, cedar, walnut, cedar, nil)
	cedar.catalogSync()
	walnut.catalogSync()
	_, strmURL = materializedStrm(t, walnutDir+"/generated", " - cedar")
	handler, err := walnut.node.RelayHandler()
	if err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewServer(handler)
	t.Cleanup(relayServer.Close)
	relayURL = relayServer.URL

	return cedar, walnut, relayURL, strmURL, media
}

func combine(configures ...func(*config.Config)) func(*config.Config) {
	return func(cfg *config.Config) {
		for _, configure := range configures {
			if configure != nil {
				configure(cfg)
			}
		}
	}
}

func TestMediaTakesTheDirectPathAndFallsBackToTCP(t *testing.T) {
	cedar, walnut, relay, strmURL, media := directPair(t, nil, nil)
	// The first play goes over TCP and starts a direct attempt meanwhile.
	if status, body := relayGet(t, relay, strmURL, "bytes=0-99"); status != http.StatusPartialContent || !bytes.Equal(body, media[:100]) {
		t.Fatalf("first play: %d, %d bytes", status, len(body))
	}
	walnut.waitForPath(t, cedar.node.NodeID(), "direct")

	before := cedar.status().Direct.ServedDirect
	if status, body := relayGet(t, relay, strmURL, ""); status != http.StatusOK || !bytes.Equal(body, media) {
		t.Fatalf("whole file over the direct path: %d, %d of %d bytes", status, len(body), len(media))
	}
	if status, body := relayGet(t, relay, strmURL, "bytes=1000-1999"); status != http.StatusPartialContent || !bytes.Equal(body, media[1000:2000]) {
		t.Fatalf("range over the direct path: %d, %d bytes", status, len(body))
	}
	if status, body := relayGet(t, relay, strmURL, "bytes=1499900-1499999"); status != http.StatusPartialContent || !bytes.Equal(body, media[1499900:]) {
		t.Fatalf("the file's last bytes over the direct path: %d, %d bytes", status, len(body))
	}
	if cedar.status().Direct.ServedDirect <= before {
		t.Fatal("cedar should have served those requests over the direct path")
	}

	// The source's policy still decides on every direct request: once cedar
	// blocks walnut, nothing more plays, direct or not.
	cedar.must(http.MethodPut, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)
	if status, _ := relayGet(t, relay, strmURL, "bytes=2000-2099"); status == http.StatusPartialContent {
		t.Fatal("a source's block must hold over the direct path")
	}
	// Nor does a blocked member get a new direct path: cedar refuses its
	// offer, and walnut stays on TCP.
	walnut.node.direct.closeAll()
	walnut.node.direct.mutex.Lock()
	walnut.node.direct.peers[cedar.node.NodeID()].failedUntil = time.Time{}
	walnut.node.direct.mutex.Unlock()
	walnut.node.direct.client(cedar.node.NodeID())
	refused := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !refused; time.Sleep(100 * time.Millisecond) {
		for _, peer := range walnut.status().Direct.Peers {
			if peer.NodeID == cedar.node.NodeID() && peer.Path == "direct" {
				t.Fatal("a blocked member was given a direct path")
			}
			refused = refused || (peer.NodeID == cedar.node.NodeID() && strings.Contains(peer.LastError, "refused"))
		}
	}
	if !refused {
		t.Fatalf("the blocked member's offer should be refused: %+v", walnut.status().Direct)
	}
	cedar.must(http.MethodDelete, "/admin/v1/blocks/"+walnut.node.NodeID(), nil, nil)
	walnut.node.direct.mutex.Lock()
	walnut.node.direct.peers[cedar.node.NodeID()].failedUntil = time.Time{}
	walnut.node.direct.mutex.Unlock()
	relayGet(t, relay, strmURL, "bytes=0-9")
	walnut.waitForPath(t, cedar.node.NodeID(), "direct")

	// The path drops: media carries on over TCP, and the node says so.
	// Paths opening and failing are audited with identifiers and outcomes
	// only: never an address. (The blocked member's offer never reaches the
	// offer route, since the listener turns it away first, so it shows as
	// walnut's failure.)
	actions := map[string]bool{}
	for _, d := range []*testDaemon{cedar, walnut} {
		events, _ := store.NewAuditRepository(d.node.database).List(context.Background(), 1000)
		for _, event := range events {
			if !strings.HasPrefix(event.Action, "direct_path_") {
				continue
			}
			actions[d.name+" "+event.Action+" "+event.Detail["outcome"]] = true
			for key, value := range event.Detail {
				if strings.Contains(value, "127.0.0.1") {
					t.Fatalf("an audit event names an address (%s): %+v", key, event)
				}
			}
		}
	}
	for _, want := range []string{"walnut direct_path_opened dialed", "cedar direct_path_opened answered", "walnut direct_path_failed dialed"} {
		if !actions[want] {
			t.Fatalf("missing audit event %q in %v", want, actions)
		}
	}

	walnut.node.direct.closeAll()
	if status, body := relayGet(t, relay, strmURL, "bytes=3000-3099"); status != http.StatusPartialContent || !bytes.Equal(body, media[3000:3100]) {
		t.Fatalf("after the direct path dropped: %d, %d bytes", status, len(body))
	}
	walnut.waitForPath(t, cedar.node.NodeID(), "tcp")
}

// C-NT-6: a node whose NAT gives each destination its own port does not
// offer its outside address, since a punch toward it cannot succeed; media
// then stays on TCP. Configured addresses are still offered.
func TestAVaryingMappingIsNotOffered(t *testing.T) {
	direct := &directPaths{node: &Node{cfg: config.Config{}}, peers: map[string]*peerPath{}}
	outside := netip.MustParseAddrPort("198.51.100.7:41641")
	direct.discovered = natpath.Result{Address: outside}
	if candidates, err := direct.candidates(); err != nil || len(candidates) != 1 || candidates[0] != outside {
		t.Fatalf("an endpoint-independent mapping should be offered: %v, %v", candidates, err)
	}
	direct.discovered.VariesByDestination = true
	if _, err := direct.candidates(); !errors.Is(err, errNoOutsideAddress) {
		t.Fatalf("a varying mapping must not be offered: %v", err)
	}
	direct.node.cfg.DirectCandidates = []string{"192.168.87.20:41641"}
	if candidates, err := direct.candidates(); err != nil || len(candidates) != 1 || candidates[0].String() != "192.168.87.20:41641" {
		t.Fatalf("a configured LAN address should still be offered: %v, %v", candidates, err)
	}
}

// C-NT-5: the source's per-destination ceiling bounds what it sends over a
// direct path exactly as over TCP, since the same handler keys it on the
// same TLS identity.
func TestTheCeilingHoldsOverTheDirectPath(t *testing.T) {
	const ceiling = 1 << 20 // bytes per second
	cedar, walnut, relay, strmURL, media := directPair(t,
		func(cfg *config.Config) { cfg.UploadCeiling = ceiling },
		func(cfg *config.Config) { cfg.HeadCacheBytes = 0 }) // every byte from the source
	relayGet(t, relay, strmURL, "bytes=0-9")
	walnut.waitForPath(t, cedar.node.NodeID(), "direct")
	before := cedar.status().Direct.ServedDirect
	began := time.Now()
	status, body := relayGet(t, relay, strmURL, "")
	elapsed := time.Since(began)
	if status != http.StatusOK || !bytes.Equal(body, media) {
		t.Fatalf("whole file: %d, %d bytes", status, len(body))
	}
	if cedar.status().Direct.ServedDirect <= before {
		t.Fatal("the file should have come over the direct path")
	}
	// 1.5 MB at 1 MB/s, less the limiter's initial burst.
	if minimum := time.Duration(float64(len(media)-ceiling) / ceiling * float64(time.Second)); elapsed < minimum {
		t.Fatalf("the ceiling did not hold over the direct path: %d bytes in %v, want at least %v", len(media), elapsed, minimum)
	}
}
