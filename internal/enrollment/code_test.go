package enrollment

import (
	"errors"
	"strings"
	"testing"

	"jellymesh/internal/grouplog"
)

func testToken(t *testing.T) Token {
	t.Helper()
	identity := newIdentity(t, "cedar")
	token, codeHash, err := NewToken("cedar.example.org:8443", identity.Fingerprint(), "group-1", grouplog.Hash{7})
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if codeHash != CodeHash(token.Secret) {
		t.Fatal("the recorded hash must be the hash of the secret")
	}
	return token
}

// C-EN-3: a short code carries the inviter's fingerprint prefix and the
// secret, survives the ways people mistype it, and pins only the inviter.
func TestShortCodeRoundTrip(t *testing.T) {
	token := testToken(t)
	code := token.ShortCode()
	if len(strings.ReplaceAll(code, "-", "")) != 29 {
		t.Fatalf("code %q should be 29 characters", code)
	}
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " "))
	typed = strings.ReplaceAll(strings.ReplaceAll(typed, "0", "o"), "1", "l")
	parsed, err := ParseShortCode(token.Address, typed)
	if err != nil {
		t.Fatalf("parse %q: %v", typed, err)
	}
	if string(parsed.Secret) != string(token.Secret) || string(parsed.FingerprintPrefix) != string(token.FingerprintPrefix) {
		t.Fatal("round trip changed the code")
	}
	if !parsed.Matches(token.Fingerprint) {
		t.Fatal("the prefix should match the inviter")
	}
	if parsed.Matches(newIdentity(t, "other").Fingerprint()) {
		t.Fatal("the prefix must not match another key")
	}
}

func TestQRRoundTrip(t *testing.T) {
	token := testToken(t)
	parsed, err := ParseQR(token.QR())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Fingerprint != token.Fingerprint || parsed.Genesis != token.Genesis || parsed.GroupID != token.GroupID || string(parsed.Secret) != string(token.Secret) {
		t.Fatal("round trip changed the invitation")
	}
	if !parsed.Matches(token.Fingerprint) {
		t.Fatal("the full fingerprint should match")
	}
}

func TestMalformedCodesAreRefused(t *testing.T) {
	token := testToken(t)
	for name, code := range map[string]string{
		"too short":  token.ShortCode()[:20],
		"not base32": strings.Repeat("U", 29),
	} {
		if _, err := ParseShortCode(token.Address, code); !errors.Is(err, ErrMalformedCode) {
			t.Errorf("%s: error = %v, want ErrMalformedCode", name, err)
		}
	}
	if _, err := ParseShortCode("", token.ShortCode()); !errors.Is(err, ErrMalformedCode) {
		t.Error("a short code needs the inviter's address")
	}
	for name, qr := range map[string]string{
		"no prefix":  "https://example.org",
		"not base64": "jellymesh:1:***",
		"empty":      "jellymesh:1:e30",
	} {
		if _, err := ParseQR(qr); !errors.Is(err, ErrMalformedCode) {
			t.Errorf("%s: error = %v, want ErrMalformedCode", name, err)
		}
	}
}
