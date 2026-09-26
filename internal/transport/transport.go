// Package transport implements the mutual-TLS authentication between
// Jellymesh nodes described in docs/design-spec.md section 8, "Transport
// authentication".
//
// Every node holds a long-term Ed25519 key and a self-signed certificate
// over it. Because the certificate is self-signed, ordinary X.509 chain
// verification proves nothing: anyone can mint a certificate that chains to
// itself. Hostname verification proves even less, since a node's identity
// is its key, not the address it happens to connect from. Both checks are
// therefore disabled here and replaced with the one check that actually
// matters: does the presented certificate's public key match a fingerprint
// this node already trusts. Authorization is by key fingerprint alone, never
// by hostname, address, or certificate chain.
package transport

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"

	"jellymesh/internal/node"
)

var (
	// ErrNoPeerCertificate means the remote side completed, or attempted,
	// a handshake without presenting any certificate at all.
	ErrNoPeerCertificate = errors.New("peer presented no certificate")

	// ErrUntrustedPeer means the peer presented a well-formed certificate,
	// but its fingerprint is not in the trust store.
	ErrUntrustedPeer = errors.New("peer certificate fingerprint is not trusted")

	// ErrPeerMismatch means the peer's fingerprint does not match the one
	// the caller expected to see, independent of whether it is trusted.
	// This only applies to the client side, which is dialing one specific
	// node rather than accepting any trusted node.
	ErrPeerMismatch = errors.New("peer certificate fingerprint does not match the expected peer")
)

// Fingerprint identifies a peer by its public key. It is an alias for
// node.Fingerprint rather than a distinct type: the wire layer and the identity
// layer must agree on exactly what a fingerprint is, and an alias makes it
// impossible for the two to drift apart or to require conversion at the
// boundary.
type Fingerprint = node.Fingerprint

// FingerprintOfCertificate is re-exported from the node package so that callers
// working at the transport layer do not need to import both.
var FingerprintOfCertificate = node.FingerprintOfCertificate

// ErrCertificateRequired is returned when a certificate carries no public key.
var ErrCertificateRequired = node.ErrCertificateRequired

// TrustStore answers whether a peer key is authorized to communicate with
// this node. The real implementation is backed by the pairwise trust
// records described in the design spec; this package only depends on the
// interface, plus a MemoryTrustStore for tests and bootstrapping.
type TrustStore interface {
	IsTrusted(Fingerprint) bool
}

// MemoryTrustStore is a concurrency-safe, in-memory TrustStore. It exists
// for tests and for bootstrapping a node before its persistent trust store
// is wired up; it keeps no record of anything beyond the current trusted
// set.
type MemoryTrustStore struct {
	mutex   sync.RWMutex
	trusted map[Fingerprint]struct{}
}

// NewMemoryTrustStore creates a MemoryTrustStore pre-populated with the
// given fingerprints.
func NewMemoryTrustStore(trusted ...Fingerprint) *MemoryTrustStore {
	store := &MemoryTrustStore{trusted: make(map[Fingerprint]struct{}, len(trusted))}
	for _, fingerprint := range trusted {
		store.trusted[fingerprint] = struct{}{}
	}
	return store
}

// Trust marks fingerprint as authorized.
func (store *MemoryTrustStore) Trust(fingerprint Fingerprint) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.trusted[fingerprint] = struct{}{}
}

// Revoke removes fingerprint from the trusted set. Any handshake already in
// flight is unaffected; only subsequent handshakes will see the revocation.
func (store *MemoryTrustStore) Revoke(fingerprint Fingerprint) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.trusted, fingerprint)
}

// IsTrusted reports whether fingerprint is currently authorized.
func (store *MemoryTrustStore) IsTrusted(fingerprint Fingerprint) bool {
	store.mutex.RLock()
	defer store.mutex.RUnlock()
	_, ok := store.trusted[fingerprint]
	return ok
}

// verifyTrustedPeerCertificate builds a VerifyPeerCertificate callback that
// rejects any presented certificate whose fingerprint is not in trust. It is
// shared by the server and client configs because "is this key trusted" is
// the same question on both sides; the client additionally pins to one
// expected fingerprint via requireFingerprint.
func verifyTrustedPeerCertificate(trust TrustStore, requireFingerprint Fingerprint, pinned bool) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCertificate
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		fingerprint, err := FingerprintOfCertificate(leaf)
		if err != nil {
			return err
		}
		if pinned && fingerprint != requireFingerprint {
			return ErrPeerMismatch
		}
		if !trust.IsTrusted(fingerprint) {
			return ErrUntrustedPeer
		}
		return nil
	}
}

// ServerTLSConfig builds the tls.Config an inbound Jellymesh listener uses
// to authenticate connecting peers.
//
// Chain verification is not applicable here: certificates are self-signed,
// so ClientCAs is left empty and ClientAuth is RequireAnyClientCert rather
// than RequireAndVerifyClientCert, which would otherwise fail every
// handshake trying to build a chain that cannot exist. VerifyPeerCertificate
// takes over that job, matching the presented certificate's fingerprint
// against trust instead. A connection that presents no certificate at all
// fails the handshake, both because the standard library enforces
// RequireAnyClientCert's "at least one certificate" rule on its own, and
// because VerifyPeerCertificate rejects an empty certificate list itself.
func ServerTLSConfig(certificate tls.Certificate, trust TrustStore) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{certificate},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifyTrustedPeerCertificate(trust, "", false),
	}
}

// ClientTLSConfig builds the tls.Config used to dial one specific,
// already-known peer identified by expected.
//
// InsecureSkipVerify is set to true, which sounds alarming in isolation, but
// it only disables Go's built-in hostname and chain verification — both of
// which are meaningless against a self-signed certificate presented over a
// bare IP or hostname that carries no authority here. It is not a "trust
// everything" setting: VerifyPeerCertificate is still installed and runs on
// every handshake. It independently pins the server to the exact fingerprint
// the caller expected (ErrPeerMismatch otherwise, even if that other key
// happens to be trusted for some other purpose) and additionally requires
// the TrustStore to consider that fingerprint trusted (ErrUntrustedPeer
// otherwise, for example if it was revoked after the caller looked it up).
// A handshake against a peer presenting no certificate fails with
// ErrNoPeerCertificate.
// A caution for callers, verified against crypto/tls in this repository's tests
// (see TestClientHandshakeMayReturnNilDespiteServerRejection): under TLS 1.3 a
// client's Handshake returns as soon as it has sent its own Finished message.
// It does not wait to learn whether the server accepted the client certificate.
// When a server rejects this node, Handshake therefore returns nil and the
// rejection only surfaces on the first Read or Write, as
// "remote error: tls: bad certificate".
//
// A dial loop must not treat a successful handshake as proof that the peer
// accepted it. Peer reachability and peer authorization are separate results,
// and only the first I/O on the connection establishes the second.
func ClientTLSConfig(certificate tls.Certificate, expected Fingerprint, trust TrustStore) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{certificate},
		InsecureSkipVerify:    true, // see doc comment: replaced by VerifyPeerCertificate below.
		VerifyPeerCertificate: verifyTrustedPeerCertificate(trust, expected, true),
	}
}

// PeerFingerprint extracts the authenticated peer identity from a completed
// TLS connection, for handlers that need to decide what a peer may access.
// It must only be called after a successful handshake; ServerTLSConfig and
// ClientTLSConfig both guarantee that by that point state.PeerCertificates
// holds exactly the certificate chain that already passed
// VerifyPeerCertificate.
func PeerFingerprint(state tls.ConnectionState) (Fingerprint, error) {
	if len(state.PeerCertificates) == 0 {
		return "", ErrNoPeerCertificate
	}
	return FingerprintOfCertificate(state.PeerCertificates[0])
}

// ErrNotEd25519 means a presented certificate does not carry an Ed25519 key,
// which every Jellymesh node identity does.
var ErrNotEd25519 = errors.New("peer certificate does not carry an Ed25519 key")

// ServerTLSConfigAuthorizingPerRequest builds the tls.Config for a listener
// whose handlers authorize each request themselves. It still requires every
// client to present a certificate with an Ed25519 key, so that each request
// carries an identity, but it accepts a key this node does not yet trust.
//
// The federation listener needs this for exactly one purpose: an invited node
// redeems its invitation before it is a member. Every other route behind such
// a listener must check the connection's fingerprint against the roster (see
// internal/federation, which enforces that for everything except enrollment).
func ServerTLSConfigAuthorizingPerRequest(certificate tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{certificate},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: requireEd25519Certificate,
	}
}

func requireEd25519Certificate(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return ErrNoPeerCertificate
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return ErrNotEd25519
	}
	return nil
}

// PeerPublicKey returns the Ed25519 key of the authenticated peer.
func PeerPublicKey(state tls.ConnectionState) (ed25519.PublicKey, error) {
	if len(state.PeerCertificates) == 0 {
		return nil, ErrNoPeerCertificate
	}
	key, ok := state.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, ErrNotEd25519
	}
	return key, nil
}

// ClientTLSConfigMatching builds a client config that accepts the server only
// if match approves its fingerprint. An invitation's short code carries only a
// prefix of the inviter's fingerprint, and this is how the joining node pins
// the inviter with it before it has anything else to trust.
func ClientTLSConfigMatching(certificate tls.Certificate, match func(Fingerprint) bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate, as in ClientTLSConfig.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return ErrNoPeerCertificate
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			fingerprint, err := FingerprintOfCertificate(leaf)
			if err != nil {
				return err
			}
			if match == nil || !match(fingerprint) {
				return ErrPeerMismatch
			}
			return nil
		},
	}
}
