// Package grouplog implements the replicated group log described in
// docs/design-spec.md section 8, "Group state and event replication".
//
// Group state (the owner, the ownership epoch, the administrators, the roster
// of members bound to their keys, and ejections) is never edited directly. It
// is the result of replaying an append-only log of signed events, so any two
// nodes holding the same log hold the same state, and a node can check every
// event it receives back to the genesis event it was given in its invitation.
//
// Three properties carry the design:
//
//   - Only the owner of the current epoch sequences events, so two events can
//     never legitimately claim the same slot. Two correctly signed events that
//     do claim one are equivocation, and are treated as evidence, not merged.
//   - Every event names the hash of the one before it, so a log fetched from
//     any peer verifies itself; a relaying peer can withhold events but cannot
//     forge or reorder them.
//   - Administrators act through proposals they sign themselves, which the
//     owner embeds. Receivers check both signatures and re-apply the role
//     rules, so the owner cannot fabricate an administrator's decision and an
//     administrator's decision cannot exceed an administrator's powers.
//
// This package is pure: it holds no network or storage dependencies, and its
// state is a function of the events given to it.
package grouplog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"jellymesh/internal/node"
)

// Kind identifies what an event does to group state.
type Kind string

const (
	KindGenesis    Kind = "genesis"
	KindAdmission  Kind = "admission"
	KindEjection   Kind = "ejection"
	KindLeave      Kind = "leave"
	KindPromote    Kind = "promote"
	KindDemote     Kind = "demote"
	KindSuccession Kind = "succession"
	// KindAddress changes the address a member advertises (#62).
	KindAddress Kind = "address"
)

// proposalKinds are the kinds whose authority comes from an embedded,
// separately signed proposal rather than from the event signer alone.
var proposalKinds = map[Kind]bool{
	KindAdmission: true,
	KindEjection:  true,
	KindLeave:     true,
	KindPromote:   true,
	KindDemote:    true,
	KindAddress:   true,
}

// Hash is a SHA-256 digest, encoded as lowercase hex on the wire so that log
// heads are readable in logs and diagnostics.
type Hash [sha256.Size]byte

func (hash Hash) String() string { return hex.EncodeToString(hash[:]) }

func (hash Hash) IsZero() bool { return hash == Hash{} }

func (hash Hash) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(hash[:])), nil
}

func (hash *Hash) UnmarshalText(text []byte) error {
	decoded, err := hex.DecodeString(string(text))
	if err != nil {
		return fmt.Errorf("decode hash: %w", err)
	}
	if len(decoded) != sha256.Size {
		return fmt.Errorf("hash has %d bytes, want %d", len(decoded), sha256.Size)
	}
	copy(hash[:], decoded)
	return nil
}

// Head identifies the last event of a log. Two nodes with equal heads hold
// the same log, because each event's hash covers the hash before it.
type Head struct {
	Epoch    uint64 `json:"epoch"`
	Sequence uint64 `json:"sequence"`
	Hash     Hash   `json:"hash"`
}

// Event is one entry in the group log.
type Event struct {
	GroupID  string    `json:"group_id"`
	Epoch    uint64    `json:"epoch"`
	Sequence uint64    `json:"sequence"`
	PrevHash Hash      `json:"prev_hash"`
	Kind     Kind      `json:"kind"`
	IssuedAt time.Time `json:"issued_at"`
	// SignerID is the owner of Epoch, or, for a succession event, the
	// claimant becoming owner. The signing key is looked up in group state
	// rather than carried here, so an event cannot introduce its own key.
	// Genesis is the one exception: it carries the founding owner's key in
	// its payload, and is anchored by the genesis hash in the invitation.
	SignerID  string `json:"signer_id"`
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

const eventDomain = "jellymesh-grouplog-event-v1\x00"

// signingBytes is the exact byte sequence an event's signature covers. Every
// variable-length field is length-prefixed so no value can shift a field
// boundary, and the domain tag keeps a signature made here from being valid
// for any other use of the same key.
func (event Event) signingBytes() []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, 128+len(event.Payload)))
	buffer.WriteString(eventDomain)
	writeField(buffer, []byte(event.GroupID))
	writeUint64(buffer, event.Epoch)
	writeUint64(buffer, event.Sequence)
	buffer.Write(event.PrevHash[:])
	writeField(buffer, []byte(event.Kind))
	writeUint64(buffer, uint64(event.IssuedAt.UTC().UnixNano()))
	writeField(buffer, []byte(event.SignerID))
	writeField(buffer, event.Payload)
	return buffer.Bytes()
}

// Hash is the event's identity in the chain. It covers every signed field,
// including PrevHash, so it commits to the whole log up to this event.
func (event Event) Hash() Hash {
	return sha256.Sum256(event.signingBytes())
}

// Head reports the log head this event would produce.
func (event Event) Head() Head {
	return Head{Epoch: event.Epoch, Sequence: event.Sequence, Hash: event.Hash()}
}

func (event Event) verify(publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, event.signingBytes(), event.Signature) {
		return ErrInvalidSignature
	}
	return nil
}

func signEvent(identity *node.Identity, event Event) (Event, error) {
	if identity == nil {
		return Event{}, errors.New("identity is required")
	}
	event.IssuedAt = event.IssuedAt.UTC()
	event.Signature = identity.Sign(event.signingBytes())
	return event, nil
}

// Marshal encodes an event for the wire or for storage. The encoding is not
// what is signed; Unmarshal followed by verification recomputes the signed
// bytes from the fields.
func (event Event) Marshal() ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshal event: %w", err)
	}
	return data, nil
}

// UnmarshalEvent decodes an event. It does not verify it; only a Log does,
// because verification needs the group state the event extends.
func UnmarshalEvent(data []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return Event{}, fmt.Errorf("unmarshal event: %w", err)
	}
	return event, nil
}

func writeField(buffer *bytes.Buffer, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	buffer.Write(length[:])
	buffer.Write(data)
}

func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	buffer.Write(encoded[:])
}

// encodeBody marshals a payload body. The bodies are plain structs with no
// maps, so encoding/json is deterministic for them; the signature covers the
// resulting bytes exactly as stored, and they are never re-marshaled.
func encodeBody(body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	return data, nil
}

func decodeBody(data []byte, body any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	return nil
}
