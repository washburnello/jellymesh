// Package events implements C-PO-9: admissions, revocations, and policy
// updates are signed group events, and a receiving node must be able to
// verify who issued one before applying it.
//
// The design spec (section 8) treats mutual TLS as sufficient authentication
// for the transport itself, but a membership event is not a request-response
// exchange between two directly connected peers: an admission or revocation
// is minted once by the owner or an administrator and then propagated,
// third-hand, to every other group member. A node that never dialed the
// issuer still has to trust the event, so the event must carry its own proof
// of who signed it and cannot lean on the transport's peer authentication.
//
// This package intentionally knows nothing about group roles, invitations,
// or the effects of admission and revocation; internal/policy owns that
// state machine and consumes Envelope only after Verify has succeeded.
package events

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"jellymesh/internal/node"
)

// Kind identifies what a signed envelope contains. It is bound into the
// signed bytes (see canonicalSigningBytes) so that a signature minted for one
// kind of event can never be presented as valid for another: an admission
// and a revocation for the same group, member, and sequence would otherwise
// carry the same group ID, issuer, sequence, and issued-at, and could differ
// only in payload shape, which is not itself covered unless Kind is signed
// alongside it.
type Kind string

const (
	KindAdmission    Kind = "admission"
	KindRevocation   Kind = "revocation"
	KindPolicyUpdate Kind = "policy_update"
)

var (
	// ErrInvalidSignature means the Ed25519 signature does not verify against
	// the canonical signing bytes recomputed from the envelope and the public
	// key handed to Verify. This covers a wrong key, a wrong signature, and
	// any tampering with a signed field, since every field that carries
	// meaning is included in what was signed.
	ErrInvalidSignature = errors.New("event envelope signature is invalid")

	// ErrIssuerMismatch means the envelope's Signature is cryptographically
	// valid for the public key it was checked against, but that key's
	// fingerprint does not equal the envelope's self-reported Issuer. A
	// signer can only produce this by lying about its own identity when it
	// signs, since Sign always records the signing identity's own
	// fingerprint; Verify catches it so a validly-signed event can never be
	// relabeled as coming from someone else.
	ErrIssuerMismatch = errors.New("event envelope issuer fingerprint does not match the verifying public key")

	// ErrUnknownKind means the envelope's Kind is not one of the declared
	// constants. An unrecognized kind must never be silently accepted, since
	// a future kind added by a newer peer could carry semantics an older
	// verifier does not understand.
	ErrUnknownKind = errors.New("event envelope kind is not recognized")

	// ErrInvalidSequence means the sequence number is zero. Zero is reserved
	// so that it can never be mistaken for "no event yet seen" by a
	// SequenceGuard, which starts every group at an implicit high-water mark
	// of zero.
	ErrInvalidSequence = errors.New("event sequence must be greater than zero")

	// ErrStaleSequence means a SequenceGuard has already admitted a sequence
	// number at or above the one presented, for the same group. This is the
	// replay guard: a captured, validly-signed event cannot be re-applied,
	// and two events cannot both claim the same slot in the group's order.
	ErrStaleSequence = errors.New("event sequence has already been seen or superseded")

	// ErrPayloadRequired means the event body is missing. A signed envelope
	// with no payload authorizes nothing, so it is rejected before it is
	// even a candidate for signing or verification.
	ErrPayloadRequired = errors.New("event payload is required")
)

// Envelope is a signed, sequenced group event: an admission, a revocation,
// or a policy update, minted once by the owner or an administrator and
// propagated to every other group member. A receiving node calls Verify
// before ever acting on Payload.
type Envelope struct {
	Kind      Kind
	GroupID   string
	IssuerID  string
	Issuer    node.Fingerprint
	Sequence  uint64
	IssuedAt  time.Time
	Payload   []byte // canonical encoding of the event body, see Sign
	Signature []byte
}

// domainSeparator prefixes every signing input for this package. Ed25519
// keys in Jellymesh are also used for mutual-TLS client certificates (see
// internal/node), and nothing stops a future signer from finding other uses
// for the same long-term key. Without a domain tag, a signature produced
// for some other purpose that happened to cover the same bytes could be
// replayed here, or a signature minted here could be replayed elsewhere.
// The trailing NUL is not load-bearing for uniqueness (the length-prefixed
// fields that follow already make the encoding unambiguous) but makes the
// boundary between the tag and the first field visually unmistakable in a
// hex dump.
const domainSeparator = "jellymesh-event-v1\x00"

// canonicalSigningBytes builds the exact byte sequence an envelope's
// signature covers. This, not any struct serialization, is the contract
// Sign and Verify must agree on byte-for-byte.
//
// Layout, in order:
//
//	domainSeparator                             (fixed bytes, see above)
//	uint32 big-endian length + Kind bytes
//	uint32 big-endian length + GroupID bytes
//	uint32 big-endian length + IssuerID bytes
//	uint32 big-endian length + Issuer fingerprint bytes
//	8 bytes big-endian Sequence
//	8 bytes big-endian IssuedAt, as UTC UnixNano (two's complement)
//	uint32 big-endian length + Payload bytes
//
// Every variable-length field is explicitly length-prefixed rather than
// joined with a delimiter, so no value (however it is chosen, including one
// containing bytes that look like a delimiter) can shift where one field
// ends and the next begins. This is deliberately not "encode a struct and
// sign that": Go does not guarantee stable field or key ordering for a map,
// and a struct encoder that changed field order between versions (or that
// contained a map anywhere in its value graph) would make an old signature
// unverifiable, or worse, make two different logical encodings hash to
// values that a less careful comparison could confuse. Building the bytes
// by hand, field by field, removes that dependency entirely.
//
// Every field that carries meaning is included: Kind (so one kind of event
// can never be presented as another), GroupID, IssuerID, and Issuer (so an
// event cannot be moved to a different group or relabeled to a different
// issuer), Sequence and IssuedAt (so ordering and timing cannot be altered
// post-signature), and Payload (the event body itself).
func canonicalSigningBytes(kind Kind, groupID string, issuerID string, issuer node.Fingerprint, sequence uint64, issuedAt time.Time, payload []byte) []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, len(domainSeparator)+64+len(payload)))
	buffer.WriteString(domainSeparator)

	appendLengthPrefixed(buffer, []byte(kind))
	appendLengthPrefixed(buffer, []byte(groupID))
	appendLengthPrefixed(buffer, []byte(issuerID))
	appendLengthPrefixed(buffer, []byte(issuer))

	var fixedWidth [16]byte
	binary.BigEndian.PutUint64(fixedWidth[0:8], sequence)
	binary.BigEndian.PutUint64(fixedWidth[8:16], uint64(issuedAt.UTC().UnixNano()))
	buffer.Write(fixedWidth[:])

	appendLengthPrefixed(buffer, payload)
	return buffer.Bytes()
}

// appendLengthPrefixed writes a uint32 big-endian byte count followed by
// data, so canonicalSigningBytes never has to rely on a delimiter that some
// field's content might itself contain.
func appendLengthPrefixed(buffer *bytes.Buffer, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	buffer.Write(length[:])
	buffer.Write(data)
}

// Sign builds and signs an Envelope on behalf of identity. The issuer fields
// (IssuerID and Issuer) are always taken from identity itself, never from a
// caller-supplied value: an event can only ever be self-attested by the key
// that actually signs it, which is what makes ErrIssuerMismatch a meaningful
// check on the verifying side rather than a formality.
//
// payload is marshaled with encoding/json to produce the canonical Payload
// bytes stored on the envelope. Verify and Decode never re-marshal it; they
// operate on those exact stored bytes, which is what makes the payload
// encoding safe to build with encoding/json here despite the general
// warning against signing a re-serialized struct: there is no second
// serialization step for the signed bytes to disagree with. The one
// obligation this places on callers is that payload's JSON encoding must
// itself be deterministic, which holds for the plain structs this package
// is used for (policy.Admission, policy.Revocation, and similar) but would
// not hold for a payload type containing a map with more than one key.
func Sign(identity *node.Identity, kind Kind, groupID string, issuerID string, sequence uint64, issuedAt time.Time, payload any) (Envelope, error) {
	if identity == nil {
		return Envelope{}, errors.New("identity is required")
	}
	if err := validateKind(kind); err != nil {
		return Envelope{}, err
	}
	if sequence == 0 {
		return Envelope{}, ErrInvalidSequence
	}
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return Envelope{}, errors.New("group ID is required")
	}
	issuerID = strings.TrimSpace(issuerID)
	if issuerID == "" {
		return Envelope{}, errors.New("issuer ID is required")
	}
	if payload == nil {
		return Envelope{}, ErrPayloadRequired
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal event payload: %w", err)
	}
	if len(payloadBytes) == 0 || string(payloadBytes) == "null" {
		return Envelope{}, ErrPayloadRequired
	}

	privateKey, err := signingKey(identity)
	if err != nil {
		return Envelope{}, err
	}

	issuedAt = issuedAt.UTC()
	issuer := identity.Fingerprint()

	signingBytes := canonicalSigningBytes(kind, groupID, issuerID, issuer, sequence, issuedAt, payloadBytes)
	signature := ed25519.Sign(privateKey, signingBytes)

	return Envelope{
		Kind:      kind,
		GroupID:   groupID,
		IssuerID:  issuerID,
		Issuer:    issuer,
		Sequence:  sequence,
		IssuedAt:  issuedAt,
		Payload:   payloadBytes,
		Signature: signature,
	}, nil
}

// signingKey returns identity's Ed25519 private key through the explicit
// accessor internal/node provides for the purpose. An earlier version reached
// through TLSCertificate().PrivateKey, which happened to work but coupled this
// package to a field documented for crypto/tls rather than for signing.
func signingKey(identity *node.Identity) (ed25519.PrivateKey, error) {
	privateKey, ok := identity.Signer().(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("identity does not hold an Ed25519 private key")
	}
	return privateKey, nil
}

// Verify checks the signature against the issuer's public key and confirms
// the envelope's issuer fingerprint matches that key.
//
// The signature is checked first: it is the strongest statement Verify can
// make, since Kind, GroupID, IssuerID, Issuer, Sequence, IssuedAt, and
// Payload are all bound into it, so a wrong key or any tampered field is
// rejected here as ErrInvalidSignature. Only once the bytes are shown to be
// genuinely signed by publicKey does Verify ask the narrower question of
// whether the signer told the truth about its own identity: Sign always
// records the signing identity's own fingerprint as Issuer, so a mismatch
// here means whoever produced this (otherwise valid) signature signed a
// false Issuer field, and the event is rejected as ErrIssuerMismatch rather
// than silently attributed to the wrong node.
func (envelope Envelope) Verify(publicKey ed25519.PublicKey) error {
	if err := validateKind(envelope.Kind); err != nil {
		return err
	}
	if envelope.Sequence == 0 {
		return ErrInvalidSequence
	}
	if len(envelope.Payload) == 0 {
		return ErrPayloadRequired
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return ErrInvalidSignature
	}

	signingBytes := canonicalSigningBytes(envelope.Kind, envelope.GroupID, envelope.IssuerID, envelope.Issuer, envelope.Sequence, envelope.IssuedAt, envelope.Payload)
	if !ed25519.Verify(publicKey, signingBytes, envelope.Signature) {
		return ErrInvalidSignature
	}

	issuerFingerprint, err := node.FingerprintOfPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("compute issuer fingerprint: %w", err)
	}
	if issuerFingerprint != envelope.Issuer {
		return ErrIssuerMismatch
	}
	return nil
}

// Decode unmarshals the payload into v after the caller has verified. It
// deliberately does not verify itself: decoding an event a caller has not
// checked the signature on is exactly the mistake this package exists to
// prevent, so callers must call Verify first and are trusted to have done
// so.
func (envelope Envelope) Decode(v any) error {
	if len(envelope.Payload) == 0 {
		return ErrPayloadRequired
	}
	if err := json.Unmarshal(envelope.Payload, v); err != nil {
		return fmt.Errorf("decode event payload: %w", err)
	}
	return nil
}

// wireEnvelope mirrors Envelope field-for-field and exists only so Marshal
// and Unmarshal have an explicit, stable JSON shape to depend on rather than
// relying on Envelope's exported field set never changing shape by
// accident. This is wire packaging, not the signing contract: the bytes
// Marshal produces are never themselves signed over, only Envelope.Payload
// is, so ordinary encoding/json is fine here even though it would not be
// for canonicalSigningBytes.
type wireEnvelope struct {
	Kind      Kind             `json:"kind"`
	GroupID   string           `json:"group_id"`
	IssuerID  string           `json:"issuer_id"`
	Issuer    node.Fingerprint `json:"issuer"`
	Sequence  uint64           `json:"sequence"`
	IssuedAt  time.Time        `json:"issued_at"`
	Payload   []byte           `json:"payload"`
	Signature []byte           `json:"signature"`
}

// Marshal moves an envelope over the wire.
func (envelope Envelope) Marshal() ([]byte, error) {
	data, err := json.Marshal(wireEnvelope(envelope))
	if err != nil {
		return nil, fmt.Errorf("marshal event envelope: %w", err)
	}
	return data, nil
}

// Unmarshal reconstructs an envelope moved over the wire by Marshal. It does
// not verify the envelope; the caller must call Verify before trusting or
// decoding it.
func Unmarshal(data []byte) (Envelope, error) {
	var wire wireEnvelope
	if err := json.Unmarshal(data, &wire); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal event envelope: %w", err)
	}
	return Envelope(wire), nil
}

func validateKind(kind Kind) error {
	switch kind {
	case KindAdmission, KindRevocation, KindPolicyUpdate:
		return nil
	default:
		return ErrUnknownKind
	}
}

// SequenceGuard is the replay guard for signed group events: it tracks, per
// group, the highest sequence number admitted so far, so that a captured
// and re-sent envelope (even one that verifies perfectly, since replaying
// changes nothing about its signature) is refused rather than re-applied.
//
// A SequenceGuard says nothing about signature validity; it is meant to be
// consulted after Verify succeeds, as the second of two independent gates
// an incoming event must pass before a caller applies it.
type SequenceGuard struct {
	mutex   sync.Mutex
	highest map[string]uint64
}

// NewSequenceGuard returns a SequenceGuard with no groups seen yet.
func NewSequenceGuard() *SequenceGuard {
	return &SequenceGuard{highest: make(map[string]uint64)}
}

// Admit returns an error for a sequence at or below the highest already
// seen for that group, and records it otherwise. Sequence zero is always
// invalid, matching Sign and Verify, so a guard can never be primed with an
// admissible sequence of zero by accident.
//
// Groups are tracked independently: a stale sequence for one group has no
// effect on another, since a single owner or administrator issues events
// for exactly one group and there is no shared ordering between groups to
// protect.
func (guard *SequenceGuard) Admit(groupID string, sequence uint64) error {
	if sequence == 0 {
		return ErrInvalidSequence
	}
	groupID = strings.TrimSpace(groupID)

	guard.mutex.Lock()
	defer guard.mutex.Unlock()
	if guard.highest == nil {
		guard.highest = make(map[string]uint64)
	}
	if sequence <= guard.highest[groupID] {
		return ErrStaleSequence
	}
	guard.highest[groupID] = sequence
	return nil
}
