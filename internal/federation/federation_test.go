package federation

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// unchecked is a service whose member route does no authorization of its own,
// as a route added carelessly later might not.
type unchecked struct{}

func (unchecked) Register(public *http.ServeMux, members *http.ServeMux) {
	public.HandleFunc("GET "+PublicPrefix+"/hello", func(response http.ResponseWriter, _ *http.Request) {
		io.WriteString(response, "public")
	})
	members.HandleFunc("GET /jellymesh/v1/secret", func(response http.ResponseWriter, _ *http.Request) {
		io.WriteString(response, "secret")
	})
	// A route registered as public outside the public prefix is unreachable.
	public.HandleFunc("GET /jellymesh/v1/misplaced", func(response http.ResponseWriter, _ *http.Request) {
		io.WriteString(response, "misplaced")
	})
}

func newIdentity(t *testing.T, name string) *node.Identity {
	t.Helper()
	directory := t.TempDir()
	identity, err := node.LoadOrCreate(filepath.Join(directory, "node.key"), filepath.Join(directory, "node.crt"), name+".example.org")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return identity
}

func get(t *testing.T, client *node.Identity, server *node.Identity, address string, path string) int {
	t.Helper()
	config := transport.ClientTLSConfig(client.TLSCertificate(), server.Fingerprint(), transport.NewMemoryTrustStore(server.Fingerprint()))
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 5 * time.Second}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+address+path, nil)
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	response.Body.Close()
	return response.StatusCode
}

// C-EN-6: routing is the authorization boundary. A member route with no check
// of its own is still private, and only the public prefix is reachable by a
// key outside every roster.
func TestOnlyThePublicPrefixIsReachableByANonMember(t *testing.T) {
	serverIdentity, member, outsider := newIdentity(t, "cedar"), newIdentity(t, "walnut"), newIdentity(t, "outsider")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: Handler(transport.NewMemoryTrustStore(member.Fingerprint()), unchecked{}), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(tls.NewListener(listener, TLSConfig(serverIdentity)))
	defer server.Close()
	address := listener.Addr().String()

	cases := []struct {
		client *node.Identity
		path   string
		want   int
	}{
		{outsider, PublicPrefix + "/hello", http.StatusOK},
		{outsider, "/jellymesh/v1/secret", http.StatusNotFound},
		{member, "/jellymesh/v1/secret", http.StatusOK},
		{member, "/jellymesh/v1/misplaced", http.StatusNotFound},
		{outsider, "/jellymesh/v1/misplaced", http.StatusNotFound},
	}
	for _, tc := range cases {
		if got := get(t, tc.client, serverIdentity, address, tc.path); got != tc.want {
			t.Errorf("%s as %s: status %d, want %d", tc.path, tc.client.Certificate().Subject.CommonName, got, tc.want)
		}
	}
}
