package grouplog

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"jellymesh/internal/node"
)

// Proposal is a decision signed by the member who made it: an administrator
// approving an admission or ejecting a member, a member leaving, or the owner
// changing the administrator set. The owner embeds it in an event to give it
// a place in the log; the proposal's own signature is what gives it authority.
type Proposal struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"group_id"`
	Kind       Kind      `json:"kind"`
	ProposerID string    `json:"proposer_id"`
	IssuedAt   time.Time `json:"issued_at"`
	Body       []byte    `json:"body"`
	Signature  []byte    `json:"signature"`
}

const proposalDomain = "jellymesh-grouplog-proposal-v1\x00"

func (proposal Proposal) signingBytes() []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, 96+len(proposal.Body)))
	buffer.WriteString(proposalDomain)
	writeField(buffer, []byte(proposal.ID))
	writeField(buffer, []byte(proposal.GroupID))
	writeField(buffer, []byte(proposal.Kind))
	writeField(buffer, []byte(proposal.ProposerID))
	writeUint64(buffer, uint64(proposal.IssuedAt.UTC().UnixNano()))
	writeField(buffer, proposal.Body)
	return buffer.Bytes()
}

func (proposal Proposal) verify(publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, proposal.signingBytes(), proposal.Signature) {
		return ErrInvalidProposalSignature
	}
	return nil
}

// NewProposal signs a proposal on behalf of proposerID. The proposal ID is
// random; the log refuses any ID it has already applied, so a proposal cannot
// be embedded twice.
func NewProposal(identity *node.Identity, groupID string, proposerID string, kind Kind, body any, issuedAt time.Time) (Proposal, error) {
	if identity == nil {
		return Proposal{}, errors.New("identity is required")
	}
	if !proposalKinds[kind] {
		return Proposal{}, ErrUnknownKind
	}
	encoded, err := encodeBody(body)
	if err != nil {
		return Proposal{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Proposal{}, err
	}
	proposal := Proposal{
		ID:         hex.EncodeToString(id[:]),
		GroupID:    strings.TrimSpace(groupID),
		Kind:       kind,
		ProposerID: strings.TrimSpace(proposerID),
		IssuedAt:   issuedAt.UTC(),
		Body:       encoded,
	}
	proposal.Signature = identity.Sign(proposal.signingBytes())
	return proposal, nil
}

// Attestation is one member's signed statement that it has been unable to
// reach the owner. Succession requires a quorum of them. Because each is
// signed by its attestor, a claimant cannot write the quorum itself.
type Attestation struct {
	GroupID          string    `json:"group_id"`
	Epoch            uint64    `json:"epoch"`
	AbsentOwnerID    string    `json:"absent_owner_id"`
	AttestorID       string    `json:"attestor_id"`
	UnreachableSince time.Time `json:"unreachable_since"`
	IssuedAt         time.Time `json:"issued_at"`
	// Head is the attestor's log head when it signed. The succession event
	// must build on a log that contains it, so a claim cannot discard events
	// that an attestor had already applied.
	Head      Head   `json:"head"`
	Signature []byte `json:"signature"`
}

const attestationDomain = "jellymesh-grouplog-attestation-v1\x00"

func (attestation Attestation) signingBytes() []byte {
	buffer := bytes.NewBuffer(make([]byte, 0, 160))
	buffer.WriteString(attestationDomain)
	writeField(buffer, []byte(attestation.GroupID))
	writeUint64(buffer, attestation.Epoch)
	writeField(buffer, []byte(attestation.AbsentOwnerID))
	writeField(buffer, []byte(attestation.AttestorID))
	writeUint64(buffer, uint64(attestation.UnreachableSince.UTC().UnixNano()))
	writeUint64(buffer, uint64(attestation.IssuedAt.UTC().UnixNano()))
	writeUint64(buffer, attestation.Head.Epoch)
	writeUint64(buffer, attestation.Head.Sequence)
	buffer.Write(attestation.Head.Hash[:])
	return buffer.Bytes()
}

func (attestation Attestation) verify(publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, attestation.signingBytes(), attestation.Signature) {
		return ErrInvalidAttestation
	}
	return nil
}

// Attest signs an absence attestation for the current owner of state, at the
// attestor's current head.
func Attest(identity *node.Identity, attestorID string, state *State, unreachableSince time.Time, issuedAt time.Time) (Attestation, error) {
	if identity == nil || state == nil {
		return Attestation{}, errors.New("identity and state are required")
	}
	attestation := Attestation{
		GroupID:          state.GroupID,
		Epoch:            state.Epoch,
		AbsentOwnerID:    state.OwnerID,
		AttestorID:       strings.TrimSpace(attestorID),
		UnreachableSince: unreachableSince.UTC(),
		IssuedAt:         issuedAt.UTC(),
		Head:             state.Head,
	}
	attestation.Signature = identity.Sign(attestation.signingBytes())
	return attestation, nil
}

// Payload bodies. Each is a plain struct with no maps, so its JSON encoding
// is deterministic.

// GenesisBody founds a group. It is the only body that carries a key the log
// has not already recorded, which is why its hash is delivered out of band.
type GenesisBody struct {
	OwnerID        string `json:"owner_id"`
	OwnerKey       []byte `json:"owner_key"`
	FriendlyName   string `json:"friendly_name"`
	PublicHostname string `json:"public_hostname"`
}

// AdmissionBody admits a node, binding its node ID to its key.
type AdmissionBody struct {
	MemberID       string `json:"member_id"`
	MemberKey      []byte `json:"member_key"`
	FriendlyName   string `json:"friendly_name"`
	PublicHostname string `json:"public_hostname"`
	InvitationID   string `json:"invitation_id"`
	InviterID      string `json:"inviter_id"`
}

// MemberBody names the member an ejection, leave, promotion, or demotion
// applies to.
type MemberBody struct {
	MemberID string `json:"member_id"`
}

// AddressBody changes the address a member advertises. A member may propose
// it only for itself.
type AddressBody struct {
	MemberID       string `json:"member_id"`
	PublicHostname string `json:"public_hostname"`
}

// SuccessionBody carries the attestations that justify a claim.
type SuccessionBody struct {
	AbsentOwnerID string        `json:"absent_owner_id"`
	Attestations  []Attestation `json:"attestations"`
}
