// Package federation assembles the public federation listener: one mutual-TLS
// socket that carries both enrollment and member traffic.
//
// The TLS layer accepts any Ed25519 client key, because an invited node must
// be able to redeem its invitation before it is a member. That makes the
// routing below the authorization boundary. Only routes under PublicPrefix
// are reachable by a key outside every roster; every other route requires the
// connection's key to belong to a member of some group this node serves, and
// its handler then checks the specific group it names. Keeping the rule here,
// rather than in each handler, means a route added later is private unless it
// is deliberately placed under PublicPrefix.
package federation

import (
	"crypto/tls"
	"net/http"
	"strings"

	"jellymesh/internal/node"
	"jellymesh/internal/transport"
)

// PublicPrefix is the only path prefix a non-member may reach.
const PublicPrefix = "/jellymesh/v1/enroll"

// Routes is implemented by each service on the listener. Public routes must
// lie under PublicPrefix; anything else registered on public is unreachable.
type Routes interface {
	Register(public *http.ServeMux, members *http.ServeMux)
}

// TLSConfig is the listener's TLS configuration.
func TLSConfig(identity *node.Identity) *tls.Config {
	return transport.ServerTLSConfigAuthorizingPerRequest(identity.TLSCertificate())
}

// Handler routes public and member traffic. trust decides membership for the
// member routes: it should accept a key that belongs to a member of any group
// this node serves.
func Handler(trust transport.TrustStore, services ...Routes) http.Handler {
	public, members := http.NewServeMux(), http.NewServeMux()
	for _, service := range services {
		service.Register(public, members)
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == PublicPrefix || strings.HasPrefix(request.URL.Path, PublicPrefix+"/") {
			public.ServeHTTP(response, request)
			return
		}
		if !trusted(trust, request) {
			http.NotFound(response, request)
			return
		}
		members.ServeHTTP(response, request)
	})
}

func trusted(trust transport.TrustStore, request *http.Request) bool {
	if trust == nil || request.TLS == nil {
		return false
	}
	fingerprint, err := transport.PeerFingerprint(*request.TLS)
	return err == nil && trust.IsTrusted(fingerprint)
}
