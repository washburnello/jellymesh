// Package enrollment admits a new node to a group (design-spec section 8,
// "Enrollment against the log").
//
// An invitation is a one-time secret plus enough to authenticate the inviter:
// its address and its key fingerprint. The joining node redeems the secret at
// the inviter over mutual TLS, pinning the inviter's key, and presents its own
// key as its client certificate. That key is what the admission will bind.
// An owner or administrator approves by signing an admission proposal, the
// owner sequences it, and the joining node then downloads the group log from
// the inviter, anchored to the genesis hash.
package enrollment

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"jellymesh/internal/grouplog"
	"jellymesh/internal/node"
)

const (
	// SecretSize is the one-time secret's length. 64 bits cannot be guessed
	// online against the redemption rate limit within an invitation's life,
	// and the secret is single-use.
	SecretSize = 8

	// FingerprintPrefixSize is how much of the inviter's fingerprint a short
	// code carries. Impersonating the inviter means finding a key whose
	// fingerprint matches these 80 bits, which is a second-preimage search
	// far beyond anyone attacking a household invitation.
	FingerprintPrefixSize = 10

	qrPrefix = "jellymesh:1:"
)

var (
	ErrMalformedCode = errors.New("invitation code is malformed")
	ErrGenesisDiffer = errors.New("the inviter's group is not the one in the invitation")
)

// crockford is Crockford's base32 alphabet, which leaves out I, L, O, and U
// so that a code read aloud or copied by hand is hard to get wrong.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Token is an invitation as the joining node holds it.
type Token struct {
	Address string
	// Fingerprint is the inviter's full fingerprint, when the invitation
	// carried it (the QR form). Otherwise only FingerprintPrefix is known.
	Fingerprint       node.Fingerprint
	FingerprintPrefix []byte
	// GroupID and Genesis are known from the QR form. With a short code they
	// are learned from the inviter over the pinned connection.
	GroupID string
	Genesis grouplog.Hash
	Secret  []byte
}

// NewToken creates an invitation for a group, returning the token to give the
// invitee and the hash the inviter records.
func NewToken(inviterAddress string, inviterFingerprint node.Fingerprint, groupID string, genesis grouplog.Hash) (Token, string, error) {
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return Token{}, "", err
	}
	prefix, err := fingerprintPrefix(inviterFingerprint)
	if err != nil {
		return Token{}, "", err
	}
	token := Token{
		Address:           strings.TrimSpace(inviterAddress),
		Fingerprint:       inviterFingerprint,
		FingerprintPrefix: prefix,
		GroupID:           groupID,
		Genesis:           genesis,
		Secret:            secret,
	}
	return token, CodeHash(secret), nil
}

// CodeHash is what the inviter stores and looks an invitation up by. The
// secret itself is never stored.
func CodeHash(secret []byte) string {
	sum := sha256.Sum256(append([]byte("jellymesh-invitation-secret\x00"), secret...))
	return hex.EncodeToString(sum[:])
}

// ShortCode renders the fingerprint prefix and secret as 29 characters in
// groups of five. It is given together with the inviter's address.
func (token Token) ShortCode() string {
	encoded := crockford.EncodeToString(append(append([]byte(nil), token.FingerprintPrefix...), token.Secret...))
	var groups []string
	for len(encoded) > 5 {
		groups = append(groups, encoded[:5])
		encoded = encoded[5:]
	}
	return strings.Join(append(groups, encoded), "-")
}

// ParseShortCode reads a short code typed by a person, tolerating case,
// spaces, dashes, and the letters Crockford's alphabet maps onto digits.
func ParseShortCode(address string, code string) (Token, error) {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t':
			return -1
		case 'O', 'o':
			return '0'
		case 'I', 'i', 'L', 'l':
			return '1'
		}
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, code)
	decoded, err := crockford.DecodeString(normalized)
	if err != nil || len(decoded) != FingerprintPrefixSize+SecretSize {
		return Token{}, ErrMalformedCode
	}
	if strings.TrimSpace(address) == "" {
		return Token{}, fmt.Errorf("%w: the inviter's address is required", ErrMalformedCode)
	}
	return Token{
		Address:           strings.TrimSpace(address),
		FingerprintPrefix: decoded[:FingerprintPrefixSize],
		Secret:            decoded[FingerprintPrefixSize:],
	}, nil
}

type qrBody struct {
	Address     string           `json:"a"`
	Fingerprint node.Fingerprint `json:"f"`
	GroupID     string           `json:"g"`
	Genesis     grouplog.Hash    `json:"h"`
	Secret      []byte           `json:"s"`
}

// QR renders the full invitation, including the inviter's whole fingerprint
// and the genesis hash, for a QR code.
func (token Token) QR() string {
	data, _ := json.Marshal(qrBody{token.Address, token.Fingerprint, token.GroupID, token.Genesis, token.Secret})
	return qrPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// ParseQR reads a scanned invitation.
func ParseQR(text string) (Token, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(text), qrPrefix)
	if !ok {
		return Token{}, ErrMalformedCode
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Token{}, ErrMalformedCode
	}
	var body qrBody
	if err := json.Unmarshal(data, &body); err != nil || len(body.Secret) != SecretSize || body.Address == "" || body.GroupID == "" || body.Genesis.IsZero() {
		return Token{}, ErrMalformedCode
	}
	prefix, err := fingerprintPrefix(body.Fingerprint)
	if err != nil {
		return Token{}, ErrMalformedCode
	}
	return Token{
		Address:           body.Address,
		Fingerprint:       body.Fingerprint,
		FingerprintPrefix: prefix,
		GroupID:           body.GroupID,
		Genesis:           body.Genesis,
		Secret:            body.Secret,
	}, nil
}

// Matches reports whether a server's fingerprint is the inviter's: exactly,
// when the token has the whole fingerprint, or by prefix otherwise.
func (token Token) Matches(fingerprint node.Fingerprint) bool {
	if token.Fingerprint != "" {
		return fingerprint == token.Fingerprint
	}
	prefix, err := fingerprintPrefix(fingerprint)
	return err == nil && len(token.FingerprintPrefix) == FingerprintPrefixSize && string(prefix) == string(token.FingerprintPrefix)
}

func fingerprintPrefix(fingerprint node.Fingerprint) ([]byte, error) {
	decoded, err := hex.DecodeString(string(fingerprint))
	if err != nil || len(decoded) < FingerprintPrefixSize {
		return nil, fmt.Errorf("%w: fingerprint", ErrMalformedCode)
	}
	return decoded[:FingerprintPrefixSize], nil
}
