// Package node owns this node's cryptographic identity: the long-term Ed25519
// key pair and the self-signed X.509 certificate that backs mutual TLS between
// Jellymesh nodes.
//
// A node's identity must outlive certificate rotation. Peers pin the node by
// the fingerprint of its public key rather than by the certificate itself, so
// this package is careful to keep the two concepts distinct: the key is
// permanent, the certificate is disposable, and only the key ever contributes
// to the fingerprint.
//
// Persisting anything beyond this key and certificate — trust decisions about
// other nodes, group membership, and so on — is a storage-layer concern that
// is still undecided and is out of scope here.
package node

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// certificateValidity is long on purpose: this certificate is not what
	// establishes trust between nodes (the fingerprint is), so there is no
	// security benefit to short-lived rotation, only operational cost.
	certificateValidity = 10 * 365 * 24 * time.Hour

	// certificateBackdate absorbs modest clock skew between nodes so a
	// certificate freshly minted on one machine is not rejected as "not yet
	// valid" by a peer whose clock runs a few minutes behind.
	certificateBackdate = 5 * time.Minute

	// privateKeyDirMode and privateKeyFileMode enforce that the identity
	// directory and key file are readable only by the owner. A node's private
	// key is a credential: anyone who can read it can impersonate the node to
	// every peer that has ever pinned its fingerprint.
	privateKeyDirMode  os.FileMode = 0o700
	privateKeyFileMode os.FileMode = 0o600

	// certificateFileMode is looser because the certificate contains only
	// public material, but it still lives inside the 0700 identity directory.
	certificateFileMode os.FileMode = 0o644
)

var (
	ErrHostnameRequired       = errors.New("hostname is required")
	ErrKeyFilePermissions     = errors.New("key file must not be readable or writable by group or other")
	ErrUnsupportedKeyType     = errors.New("only Ed25519 keys are supported")
	ErrCertificateKeyMismatch = errors.New("certificate public key does not match the node's private key")
	ErrInvalidPublicKey       = errors.New("public key has the wrong length for Ed25519")
	ErrCertificateRequired    = errors.New("a certificate with a public key is required")
)

// Fingerprint identifies a node by its public key. It is the lowercase hex
// encoding of SHA-256 over the certificate's DER SubjectPublicKeyInfo, with no
// separators. It MUST be computed from the public key, never from the whole
// certificate, so that a node keeps its identity across certificate renewal.
type Fingerprint string

// FingerprintOfPublicKey computes the fingerprint directly from a raw Ed25519
// public key. It is the primitive that LoadOrCreate and FingerprintOfCertificate
// both reduce to, since it does not require a certificate to exist yet.
func FingerprintOfPublicKey(pub ed25519.PublicKey) (Fingerprint, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", ErrInvalidPublicKey
	}
	// x509.MarshalPKIXPublicKey produces the same DER SubjectPublicKeyInfo
	// encoding that x509.CreateCertificate embeds in a certificate's
	// RawSubjectPublicKeyInfo, so this agrees with FingerprintOfCertificate for
	// the same key without ever needing a certificate.
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return Fingerprint(hex.EncodeToString(sum[:])), nil
}

// FingerprintOfCertificate computes the fingerprint from a certificate's
// embedded public key. It deliberately hashes only RawSubjectPublicKeyInfo,
// not the certificate as a whole: the serial number, validity window, and
// signature all change when a certificate is renewed, and none of that may
// leak into the identity a peer has pinned.
func FingerprintOfCertificate(cert *x509.Certificate) (Fingerprint, error) {
	if cert == nil || len(cert.RawSubjectPublicKeyInfo) == 0 {
		return "", ErrCertificateRequired
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return Fingerprint(hex.EncodeToString(sum[:])), nil
}

// Identity is this node's long-term Ed25519 key plus the self-signed X.509
// certificate presented to peers over mutual TLS.
type Identity struct {
	privateKey     ed25519.PrivateKey
	certificate    *x509.Certificate
	tlsCertificate tls.Certificate
	fingerprint    Fingerprint
}

// LoadOrCreate loads the identity from keyPath/certPath, creating both on
// first run. It is idempotent: a second call against the same paths returns
// the same key and fingerprint.
//
// If the certificate already exists but was not issued for the loaded
// private key, LoadOrCreate refuses to silently regenerate it: a mismatch
// almost always means the two paths were mixed up between nodes, and papering
// over that would hand out a certificate that says one thing while the key
// underneath it says another.
func LoadOrCreate(keyPath, certPath, hostname string) (*Identity, error) {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return nil, ErrHostnameRequired
	}

	privateKey, err := loadOrCreatePrivateKey(keyPath)
	if err != nil {
		return nil, err
	}

	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, ErrUnsupportedKeyType
	}

	certificate, err := loadOrCreateCertificate(certPath, privateKey, publicKey, hostname)
	if err != nil {
		return nil, err
	}

	fingerprint, err := FingerprintOfPublicKey(publicKey)
	if err != nil {
		return nil, err
	}

	return &Identity{
		privateKey:  privateKey,
		certificate: certificate,
		fingerprint: fingerprint,
		tlsCertificate: tls.Certificate{
			Certificate: [][]byte{certificate.Raw},
			PrivateKey:  privateKey,
			Leaf:        certificate,
		},
	}, nil
}

// Fingerprint returns the identity's stable fingerprint, derived from its
// public key.
func (identity *Identity) Fingerprint() Fingerprint {
	return identity.fingerprint
}

// PublicKey returns the node's Ed25519 public key.
func (identity *Identity) PublicKey() ed25519.PublicKey {
	return identity.privateKey.Public().(ed25519.PublicKey)
}

// Signer exposes the node's private key as a crypto.Signer so that other
// packages can sign group events without reaching through the TLS certificate
// to find the key. The TLS certificate is documented for use by crypto/tls;
// treating it as a general key accessor couples callers to an internal detail
// that could reasonably change.
func (identity *Identity) Signer() crypto.Signer {
	return identity.privateKey
}

// Sign produces an Ed25519 signature over message using the node's long-term
// identity key. Ed25519 signs the message itself rather than a pre-computed
// digest.
func (identity *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(identity.privateKey, message)
}

// TLSCertificate returns the certificate in the form crypto/tls expects for
// both tls.Config.Certificates (serving) and tls.Config.GetClientCertificate
// (dialing), since this node presents the same certificate in either role.
func (identity *Identity) TLSCertificate() tls.Certificate {
	return identity.tlsCertificate
}

// Certificate returns the parsed self-signed certificate.
func (identity *Identity) Certificate() *x509.Certificate {
	return identity.certificate
}

// loadOrCreatePrivateKey reads the Ed25519 private key at path, generating and
// persisting a new one if the file does not yet exist.
func loadOrCreatePrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return generatePrivateKey(path)
	case err != nil:
		return nil, fmt.Errorf("stat key file: %w", err)
	}

	// A node identity is a credential. If the key file has ever been made
	// group- or world-readable, treat that as a compromise rather than an
	// inconvenience: refuse to load it instead of quietly trusting it.
	if info.Mode().Perm()&0o077 != 0 {
		return nil, ErrKeyFilePermissions
	}

	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("key file does not contain a PEM-encoded PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, ErrUnsupportedKeyType
	}
	return privateKey, nil
}

// generatePrivateKey creates a fresh Ed25519 key and writes it to path as a
// PKCS#8 PEM file, atomically and with owner-only permissions.
func generatePrivateKey(path string) (ed25519.PrivateKey, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeFileAtomic(path, pemBytes, privateKeyFileMode); err != nil {
		return nil, fmt.Errorf("write key file: %w", err)
	}
	return privateKey, nil
}

// loadOrCreateCertificate reads the self-signed certificate at path,
// generating and persisting a new one for privateKey if the file does not yet
// exist. An existing certificate whose public key does not match privateKey
// is rejected rather than replaced.
func loadOrCreateCertificate(path string, privateKey ed25519.PrivateKey, publicKey ed25519.PublicKey, hostname string) (*x509.Certificate, error) {
	pemBytes, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return generateCertificate(path, privateKey, hostname)
	case err != nil:
		return nil, fmt.Errorf("read certificate file: %w", err)
	}

	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate file does not contain a PEM-encoded certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	certificatePublicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, ErrUnsupportedKeyType
	}
	if !certificatePublicKey.Equal(publicKey) {
		return nil, ErrCertificateKeyMismatch
	}
	return certificate, nil
}

// generateCertificate mints a self-signed certificate for privateKey and
// persists it to path, atomically.
func generateCertificate(path string, privateKey ed25519.PrivateKey, hostname string) (*x509.Certificate, error) {
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: hostname},
		DNSNames:              []string{hostname},
		NotBefore:             now.Add(-certificateBackdate),
		NotAfter:              now.Add(certificateValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFileAtomic(path, pemBytes, certificateFileMode); err != nil {
		return nil, fmt.Errorf("write certificate file: %w", err)
	}
	// Re-parse rather than reuse template: Certificate.Raw and the other
	// derived fields (like RawSubjectPublicKeyInfo) are only populated by
	// parsing the DER, and callers rely on them.
	return x509.ParseCertificate(der)
}

// writeFileAtomic writes data to path without ever leaving a truncated or
// partially-written file in its place, even if the process is killed mid-write.
// It does this by writing to a temp file in the same directory, syncing it to
// disk, and only then renaming it over the destination — rename within a
// directory is atomic, so a reader always sees either the old file or the
// complete new one, never a partial one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateKeyDirMode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	// MkdirAll's mode is masked by the process umask, so a restrictive mode
	// must be re-applied explicitly to guarantee the directory ends up 0700.
	if err := os.Chmod(dir, privateKeyDirMode); err != nil {
		return fmt.Errorf("set directory permissions: %w", err)
	}

	tempFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	// Once the rename below succeeds this is a harmless no-op error against a
	// path that no longer exists; it only matters on the early-return paths.
	defer os.Remove(tempPath)

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tempFile.Chmod(mode); err != nil {
		tempFile.Close()
		return fmt.Errorf("set temp file permissions: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		tempFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file into place: %w", err)
	}

	// Fsync the directory too, so the rename itself survives a crash. Without
	// this, a power loss right after rename could leave the directory entry
	// pointing at the old file even though the rename appeared to succeed.
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
