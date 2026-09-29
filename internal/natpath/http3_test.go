package natpath

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"jellymesh/internal/node"
)

// connected returns an HTTP client for walnut and the server connection
// cedar accepted, over a direct path.
func connected(t *testing.T, handler http.Handler) (*http.Client, *node.Identity) {
	t.Helper()
	cedarID, walnutID := identity(t, "cedar"), identity(t, "walnut")
	cedar, walnut := endpoint(t, cedarID), endpoint(t, walnutID)
	ctx := within(t, 10*time.Second)
	wait := cedar.Expect(ctx, walnutID.Fingerprint())
	dialed, err := walnut.Dial(ctx, cedarID.Fingerprint(), []netip.AddrPort{cedar.LocalAddr()})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := wait()
	if err != nil {
		t.Fatal(err)
	}
	go Serve(accepted, handler)
	return &http.Client{Transport: RoundTripper(dialed)}, walnutID
}

// C-NT-5: over a direct path, the source's handler sees the caller's pinned
// key as on TCP, and ranges and HEAD behave as HTTP defines them.
func TestMediaRequestsKeepTheirMeaningOverADirectPath(t *testing.T) {
	blob := make([]byte, 5<<20)
	rand.New(rand.NewSource(7)).Read(blob)
	var seen node.Fingerprint
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
			http.Error(response, "no identity", http.StatusForbidden)
			return
		}
		seen, _ = node.FingerprintOfCertificate(request.TLS.PeerCertificates[0])
		http.ServeContent(response, request, "film.mkv", time.Time{}, bytes.NewReader(blob))
	})
	client, walnutID := connected(t, handler)

	request, _ := http.NewRequest(http.MethodGet, "https://cedar/jellymesh/v1/groups/g/media/item", nil)
	request.Header.Set("Range", "bytes=1000000-1000099")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusPartialContent || !bytes.Equal(body, blob[1000000:1000100]) ||
		response.Header.Get("Content-Range") != "bytes 1000000-1000099/5242880" {
		t.Fatalf("range: %d %q, %d bytes", response.StatusCode, response.Header.Get("Content-Range"), len(body))
	}
	if seen != walnutID.Fingerprint() {
		t.Fatal("the handler must see the caller's pinned key")
	}

	head, _ := http.NewRequest(http.MethodHead, "https://cedar/jellymesh/v1/groups/g/media/item", nil)
	response, err = client.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(len(blob)) {
		t.Fatalf("HEAD: %d, length %d", response.StatusCode, response.ContentLength)
	}
}

// C-NT-5: when the destination stops reading, the source's handler learns
// of it and stops, as it does on TCP.
func TestCancellingADirectStreamStopsTheSource(t *testing.T) {
	stopped := make(chan struct{})
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for {
			if _, err := response.Write(chunk); err != nil {
				close(stopped)
				return
			}
			select {
			case <-request.Context().Done():
				close(stopped)
				return
			default:
			}
		}
	})
	client, _ := connected(t, handler)
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://cedar/stream", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(response.Body, make([]byte, 1<<20))
	cancel()
	response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the source kept writing after the destination cancelled")
	}
}
