package node

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func paths(t *testing.T) (keyPath string, certPath string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt")
}

func TestLoadOrCreateIsIdempotent(t *testing.T) {
	keyPath, certPath := paths(t)

	first, err := LoadOrCreate(keyPath, certPath, "alice.jellymesh.internal")
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	second, err := LoadOrCreate(keyPath, certPath, "alice.jellymesh.internal")
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("fingerprint changed across loads: %s != %s", first.Fingerprint(), second.Fingerprint())
	}
	if !first.PublicKey().Equal(second.PublicKey()) {
		t.Fatalf("public key changed across loads")
	}
}

func TestFingerprintSurvivesCertificateRenewal(t *testing.T) {
	keyPath, certPath := paths(t)

	original, err := LoadOrCreate(keyPath, certPath, "bob.jellymesh.internal")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Remove only the certificate. Reloading against the same key must mint a
	// brand new certificate (different serial, different validity window) but
	// the fingerprint must be unchanged, since it is derived from the key.
	if err := os.Remove(certPath); err != nil {
		t.Fatalf("remove certificate: %v", err)
	}

	renewed, err := LoadOrCreate(keyPath, certPath, "bob.jellymesh.internal")
	if err != nil {
		t.Fatalf("LoadOrCreate after cert removal: %v", err)
	}

	if renewed.Certificate().SerialNumber.Cmp(original.Certificate().SerialNumber) == 0 {
		t.Fatalf("expected a freshly generated certificate with a different serial number")
	}
	if renewed.Fingerprint() != original.Fingerprint() {
		t.Fatalf("fingerprint changed after certificate renewal: %s != %s", original.Fingerprint(), renewed.Fingerprint())
	}

	wantFingerprint, err := FingerprintOfPublicKey(original.PublicKey())
	if err != nil {
		t.Fatalf("FingerprintOfPublicKey: %v", err)
	}
	gotFingerprint, err := FingerprintOfCertificate(renewed.Certificate())
	if err != nil {
		t.Fatalf("FingerprintOfCertificate: %v", err)
	}
	if gotFingerprint != wantFingerprint {
		t.Fatalf("FingerprintOfCertificate disagrees with FingerprintOfPublicKey: %s != %s", gotFingerprint, wantFingerprint)
	}
}

func TestKeyFileModeIsOwnerOnly(t *testing.T) {
	keyPath, certPath := paths(t)

	if _, err := LoadOrCreate(keyPath, certPath, "carol.jellymesh.internal"); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if got := info.Mode().Perm(); got != privateKeyFileMode {
		t.Fatalf("key file mode = %o, want %o", got, privateKeyFileMode)
	}
}

func TestLoadRejectsGroupReadableKeyFile(t *testing.T) {
	keyPath, certPath := paths(t)

	if _, err := LoadOrCreate(keyPath, certPath, "dave.jellymesh.internal"); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod key file: %v", err)
	}

	if _, err := LoadOrCreate(keyPath, certPath, "dave.jellymesh.internal"); !errors.Is(err, ErrKeyFilePermissions) {
		t.Fatalf("LoadOrCreate with a 0644 key file: got %v, want ErrKeyFilePermissions", err)
	}
}

func TestLoadRejectsMismatchedCertificate(t *testing.T) {
	keyPathA, certPathA := paths(t)
	keyPathB, certPathB := paths(t)

	if _, err := LoadOrCreate(keyPathA, certPathA, "erin.jellymesh.internal"); err != nil {
		t.Fatalf("LoadOrCreate A: %v", err)
	}
	if _, err := LoadOrCreate(keyPathB, certPathB, "erin.jellymesh.internal"); err != nil {
		t.Fatalf("LoadOrCreate B: %v", err)
	}

	// Graft A's certificate onto B's key path. Since B's private key already
	// exists on disk, this simulates the certificate and key paths being
	// mixed up between two different node identities.
	certA, err := os.ReadFile(certPathA)
	if err != nil {
		t.Fatalf("read certificate A: %v", err)
	}
	if err := os.WriteFile(certPathB, certA, certificateFileMode); err != nil {
		t.Fatalf("overwrite certificate B: %v", err)
	}

	if _, err := LoadOrCreate(keyPathB, certPathB, "erin.jellymesh.internal"); !errors.Is(err, ErrCertificateKeyMismatch) {
		t.Fatalf("LoadOrCreate with mismatched cert/key: got %v, want ErrCertificateKeyMismatch", err)
	}
}

func TestLoadOrCreateRejectsEmptyHostname(t *testing.T) {
	keyPath, certPath := paths(t)

	if _, err := LoadOrCreate(keyPath, certPath, ""); !errors.Is(err, ErrHostnameRequired) {
		t.Fatalf("LoadOrCreate with empty hostname: got %v, want ErrHostnameRequired", err)
	}
	if _, err := LoadOrCreate(keyPath, certPath, "   "); !errors.Is(err, ErrHostnameRequired) {
		t.Fatalf("LoadOrCreate with whitespace hostname: got %v, want ErrHostnameRequired", err)
	}
}

func TestGeneratedCertificateCarriesBothExtKeyUsages(t *testing.T) {
	keyPath, certPath := paths(t)

	identity, err := LoadOrCreate(keyPath, certPath, "frank.jellymesh.internal")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	certificate := identity.Certificate()
	if certificate.IsCA {
		t.Fatalf("certificate must not be a CA")
	}
	if certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Fatalf("certificate missing KeyUsageDigitalSignature")
	}
	if certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("certificate missing KeyUsageCertSign")
	}

	var hasServerAuth, hasClientAuth bool
	for _, usage := range certificate.ExtKeyUsage {
		switch usage {
		case x509.ExtKeyUsageServerAuth:
			hasServerAuth = true
		case x509.ExtKeyUsageClientAuth:
			hasClientAuth = true
		}
	}
	if !hasServerAuth {
		t.Fatalf("certificate missing ExtKeyUsageServerAuth")
	}
	if !hasClientAuth {
		t.Fatalf("certificate missing ExtKeyUsageClientAuth")
	}

	if len(certificate.DNSNames) != 1 || certificate.DNSNames[0] != "frank.jellymesh.internal" {
		t.Fatalf("unexpected DNS SANs: %v", certificate.DNSNames)
	}
	if certificate.Subject.CommonName != "frank.jellymesh.internal" {
		t.Fatalf("unexpected CommonName: %s", certificate.Subject.CommonName)
	}

	tlsCert := identity.TLSCertificate()
	if len(tlsCert.Certificate) != 1 {
		t.Fatalf("expected exactly one certificate in the TLS chain")
	}
}
