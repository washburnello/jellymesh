package grouplog

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"jellymesh/internal/group"
	"jellymesh/internal/node"
)

var (
	ErrInvalidSignature         = errors.New("event signature is invalid")
	ErrInvalidProposalSignature = errors.New("proposal signature is invalid")
	ErrInvalidAttestation       = errors.New("attestation is invalid")
	ErrMalformedPayload         = errors.New("event payload is malformed")
	ErrUnknownKind              = errors.New("event kind is not recognized")
	ErrWrongGroup               = errors.New("event is for a different group")
	ErrNotGenesis               = errors.New("the first event of a log must be a genesis event")
	ErrUnexpectedGenesis        = errors.New("a genesis event may only begin a log")
	ErrWrongEpoch               = errors.New("event epoch does not match the log")
	ErrWrongSigner              = errors.New("event is not signed by the owner of its epoch")
	ErrProposalMismatch         = errors.New("embedded proposal does not match the event")
	ErrProposalReplayed         = errors.New("proposal has already been applied")
	ErrNotAuthorized            = errors.New("proposer is not permitted to make this decision")
	ErrNotMember                = errors.New("node is not an active member")
	ErrAlreadyMember            = errors.New("node is already an active member")
	ErrOwnerProtected           = errors.New("the owner cannot be ejected, demoted, or leave")
	ErrAdminProtected           = errors.New("only the owner can eject an administrator")
	ErrAlreadyAdministrator     = errors.New("member is already an administrator")
	ErrNotAdministrator         = errors.New("member is not an administrator")
	ErrInvitationConsumed       = errors.New("invitation has already been used for an admission")
	ErrFingerprintInUse         = errors.New("key already belongs to a different node")
	ErrFingerprintChanged       = errors.New("node was admitted with a different key; a new key is a new node")
	ErrNotSuccessor             = errors.New("claimant is not the eligible successor")
	ErrInsufficientAttestations = errors.New("succession requires attestations from a quorum of members")
	ErrInvalidAddress           = errors.New("an advertised address must be a host and port")
)

// Member is an active member of the group, bound to its key.
type Member struct {
	NodeID         string
	PublicKey      ed25519.PublicKey
	Fingerprint    node.Fingerprint
	FriendlyName   string
	PublicHostname string
	// AdmittedAt is the sequence of the admission (or genesis) event.
	AdmittedAt uint64
}

// State is group state as derived from a log. It is only ever produced by
// applying events in order; nothing outside this package mutates it, and
// accessors return copies where a caller could otherwise alter it.
type State struct {
	GroupID string
	Epoch   uint64
	OwnerID string
	Head    Head

	members map[string]Member
	// admins maps an administrator to the sequence of its promotion. Seniority
	// for succession is by that sequence, which every node agrees on, rather
	// than by a wall-clock time that each node would read differently.
	admins map[string]uint64
	// ejected records nodes removed by ejection, as distinct from those that
	// left, so a stale grant is never silently reinstated.
	ejected map[string]bool
	// bindings records every node ID ever admitted and the key it was admitted
	// with. A binding is never replaced: a node with a new key is a new node.
	bindings    map[string]node.Fingerprint
	boundTo     map[node.Fingerprint]string
	invitations map[string]bool
	proposals   map[string]bool
	// hashes[i] is the hash of the event with sequence i+1, so that an
	// attestation's head can be checked against this log's history.
	hashes []Hash
}

func newState() *State {
	return &State{
		members:     make(map[string]Member),
		admins:      make(map[string]uint64),
		ejected:     make(map[string]bool),
		bindings:    make(map[string]node.Fingerprint),
		boundTo:     make(map[node.Fingerprint]string),
		invitations: make(map[string]bool),
		proposals:   make(map[string]bool),
	}
}

func (state *State) IsMember(nodeID string) bool {
	_, ok := state.members[strings.TrimSpace(nodeID)]
	return ok
}

func (state *State) IsOwner(nodeID string) bool {
	return state.OwnerID != "" && state.OwnerID == strings.TrimSpace(nodeID)
}

// IsAdministrator reports whether nodeID holds administrator powers, which
// the owner always does.
func (state *State) IsAdministrator(nodeID string) bool {
	nodeID = strings.TrimSpace(nodeID)
	_, ok := state.admins[nodeID]
	return ok || state.IsOwner(nodeID)
}

func (state *State) IsEjected(nodeID string) bool {
	return state.ejected[strings.TrimSpace(nodeID)]
}

// Member returns the active member nodeID.
func (state *State) Member(nodeID string) (Member, bool) {
	member, ok := state.members[strings.TrimSpace(nodeID)]
	return member, ok
}

// MemberByFingerprint returns the active member holding fingerprint. This is
// the question transport trust asks: a connection is from a member exactly
// when its key belongs to one.
func (state *State) MemberByFingerprint(fingerprint node.Fingerprint) (Member, bool) {
	nodeID, ok := state.boundTo[fingerprint]
	if !ok {
		return Member{}, false
	}
	return state.Member(nodeID)
}

// Members returns the active members ordered by node ID.
func (state *State) Members() []Member {
	members := make([]Member, 0, len(state.members))
	for _, member := range state.members {
		members = append(members, member)
	}
	sort.Slice(members, func(left, right int) bool { return members[left].NodeID < members[right].NodeID })
	return members
}

// Administrators returns the administrators, excluding the owner, in order of
// seniority.
func (state *State) Administrators() []string {
	ids := make([]string, 0, len(state.admins))
	for id := range state.admins {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return state.admins[ids[left]] < state.admins[ids[right]] })
	return ids
}

// EligibleSuccessor is the administrator who may claim ownership: the one
// promoted earliest in the log. Promotions are totally ordered by sequence,
// so there are no ties to break.
func (state *State) EligibleSuccessor() (string, bool) {
	administrators := state.Administrators()
	if len(administrators) == 0 {
		return "", false
	}
	return administrators[0], true
}

// hashAt returns the hash of the event at sequence, if this log has one.
func (state *State) hashAt(sequence uint64) (Hash, bool) {
	if sequence == 0 || sequence > uint64(len(state.hashes)) {
		return Hash{}, false
	}
	return state.hashes[sequence-1], true
}

// apply validates event against state and, only if every check passes,
// applies it. A refused event leaves state exactly as it was.
func (state *State) apply(event Event) error {
	if state.Head.Sequence == 0 {
		return state.applyGenesis(event)
	}
	if event.Kind == KindGenesis {
		return ErrUnexpectedGenesis
	}
	if event.GroupID != state.GroupID {
		return ErrWrongGroup
	}
	if event.Epoch < state.Epoch {
		return fmt.Errorf("%w: epoch %d, log is at epoch %d", ErrFenced, event.Epoch, state.Epoch)
	}
	if event.Sequence != state.Head.Sequence+1 || event.PrevHash != state.Head.Hash {
		return fmt.Errorf("%w: event %d does not extend head %d", ErrDoesNotExtend, event.Sequence, state.Head.Sequence)
	}

	var effect func()
	var err error
	if event.Kind == KindSuccession {
		effect, err = state.validateSuccession(event)
	} else {
		effect, err = state.validateSequenced(event)
	}
	if err != nil {
		return err
	}
	effect()
	state.advance(event)
	return nil
}

func (state *State) advance(event Event) {
	hash := event.Hash()
	state.hashes = append(state.hashes, hash)
	state.Head = Head{Epoch: event.Epoch, Sequence: event.Sequence, Hash: hash}
}

func (state *State) applyGenesis(event Event) error {
	if event.Kind != KindGenesis {
		return ErrNotGenesis
	}
	if event.Sequence != 1 || event.Epoch != 1 || !event.PrevHash.IsZero() {
		return fmt.Errorf("%w: genesis must be epoch 1, sequence 1, with no previous hash", ErrNotGenesis)
	}
	if strings.TrimSpace(event.GroupID) == "" {
		return fmt.Errorf("%w: group ID is required", ErrMalformedPayload)
	}
	var body GenesisBody
	if err := decodeBody(event.Payload, &body); err != nil {
		return err
	}
	if body.OwnerID == "" || body.OwnerID != event.SignerID {
		return ErrWrongSigner
	}
	fingerprint, err := node.FingerprintOfPublicKey(body.OwnerKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}
	if err := event.verify(body.OwnerKey); err != nil {
		return err
	}

	state.GroupID = event.GroupID
	state.Epoch = 1
	state.OwnerID = body.OwnerID
	state.members[body.OwnerID] = Member{
		NodeID:         body.OwnerID,
		PublicKey:      ed25519.PublicKey(body.OwnerKey),
		Fingerprint:    fingerprint,
		FriendlyName:   body.FriendlyName,
		PublicHostname: body.PublicHostname,
		AdmittedAt:     1,
	}
	state.bindings[body.OwnerID] = fingerprint
	state.boundTo[fingerprint] = body.OwnerID
	state.advance(event)
	return nil
}

// validateSequenced checks an owner-sequenced event carrying a proposal. The
// owner's signature orders it; the proposal's signature authorizes it.
func (state *State) validateSequenced(event Event) (func(), error) {
	if !proposalKinds[event.Kind] {
		return nil, ErrUnknownKind
	}
	if event.Epoch != state.Epoch {
		return nil, ErrWrongEpoch
	}
	if event.SignerID != state.OwnerID {
		return nil, ErrWrongSigner
	}
	if err := event.verify(state.members[state.OwnerID].PublicKey); err != nil {
		return nil, err
	}

	var proposal Proposal
	if err := decodeBody(event.Payload, &proposal); err != nil {
		return nil, err
	}
	if proposal.Kind != event.Kind || proposal.GroupID != state.GroupID || proposal.ID == "" {
		return nil, ErrProposalMismatch
	}
	if state.proposals[proposal.ID] {
		return nil, ErrProposalReplayed
	}
	proposer, ok := state.members[proposal.ProposerID]
	if !ok {
		return nil, fmt.Errorf("%w: proposer %q", ErrNotMember, proposal.ProposerID)
	}
	if err := proposal.verify(proposer.PublicKey); err != nil {
		return nil, err
	}

	effect, err := state.validateDecision(event.Sequence, proposal)
	if err != nil {
		return nil, err
	}
	return func() {
		effect()
		state.proposals[proposal.ID] = true
	}, nil
}

// validateDecision applies the role rules to a verified proposal. These are
// the same rules on every node, so an owner that sequences a decision the
// rules forbid produces an event every other member refuses.
func (state *State) validateDecision(sequence uint64, proposal Proposal) (func(), error) {
	proposerID := proposal.ProposerID
	switch proposal.Kind {
	case KindAdmission:
		var body AdmissionBody
		if err := decodeBody(proposal.Body, &body); err != nil {
			return nil, err
		}
		if !state.IsAdministrator(proposerID) {
			return nil, ErrNotAuthorized
		}
		if body.MemberID == "" || body.InvitationID == "" {
			return nil, fmt.Errorf("%w: admission needs a member and an invitation", ErrMalformedPayload)
		}
		if state.IsMember(body.MemberID) {
			return nil, ErrAlreadyMember
		}
		if state.invitations[body.InvitationID] {
			return nil, ErrInvitationConsumed
		}
		fingerprint, err := node.FingerprintOfPublicKey(body.MemberKey)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
		}
		if bound, ok := state.bindings[body.MemberID]; ok && bound != fingerprint {
			return nil, ErrFingerprintChanged
		}
		if holder, ok := state.boundTo[fingerprint]; ok && holder != body.MemberID {
			return nil, ErrFingerprintInUse
		}
		return func() {
			state.members[body.MemberID] = Member{
				NodeID:         body.MemberID,
				PublicKey:      ed25519.PublicKey(body.MemberKey),
				Fingerprint:    fingerprint,
				FriendlyName:   body.FriendlyName,
				PublicHostname: body.PublicHostname,
				AdmittedAt:     sequence,
			}
			state.bindings[body.MemberID] = fingerprint
			state.boundTo[fingerprint] = body.MemberID
			state.invitations[body.InvitationID] = true
			delete(state.ejected, body.MemberID)
		}, nil

	case KindEjection:
		body, err := state.memberBody(proposal)
		if err != nil {
			return nil, err
		}
		if !state.IsAdministrator(proposerID) {
			return nil, ErrNotAuthorized
		}
		if state.IsOwner(body.MemberID) {
			return nil, ErrOwnerProtected
		}
		if _, isAdmin := state.admins[body.MemberID]; isAdmin && !state.IsOwner(proposerID) {
			return nil, ErrAdminProtected
		}
		return func() {
			state.remove(body.MemberID)
			state.ejected[body.MemberID] = true
		}, nil

	case KindLeave:
		body, err := state.memberBody(proposal)
		if err != nil {
			return nil, err
		}
		if proposerID != body.MemberID {
			return nil, ErrNotAuthorized
		}
		if state.IsOwner(body.MemberID) {
			return nil, ErrOwnerProtected
		}
		return func() { state.remove(body.MemberID) }, nil

	case KindPromote:
		body, err := state.memberBody(proposal)
		if err != nil {
			return nil, err
		}
		if !state.IsOwner(proposerID) {
			return nil, ErrNotAuthorized
		}
		if state.IsAdministrator(body.MemberID) {
			return nil, ErrAlreadyAdministrator
		}
		return func() { state.admins[body.MemberID] = sequence }, nil

	case KindAddress:
		var body AddressBody
		if err := decodeBody(proposal.Body, &body); err != nil {
			return nil, err
		}
		if !state.IsMember(body.MemberID) {
			return nil, fmt.Errorf("%w: %q", ErrNotMember, body.MemberID)
		}
		if proposerID != body.MemberID {
			return nil, ErrNotAuthorized
		}
		if !ValidAddress(body.PublicHostname) {
			return nil, ErrInvalidAddress
		}
		return func() {
			member := state.members[body.MemberID]
			member.PublicHostname = body.PublicHostname
			state.members[body.MemberID] = member
		}, nil

	case KindDemote:
		body, err := state.memberBody(proposal)
		if err != nil {
			return nil, err
		}
		if !state.IsOwner(proposerID) {
			return nil, ErrNotAuthorized
		}
		if state.IsOwner(body.MemberID) {
			return nil, ErrOwnerProtected
		}
		if _, ok := state.admins[body.MemberID]; !ok {
			return nil, ErrNotAdministrator
		}
		return func() { delete(state.admins, body.MemberID) }, nil
	}
	return nil, ErrUnknownKind
}

// ValidAddress reports whether address is a host and port, with nothing
// else, that a member may advertise.
func ValidAddress(address string) bool {
	if address == "" || len(address) > 255 || strings.ContainsAny(address, " \t\r\n/@?#") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	number, err := strconv.Atoi(port)
	return err == nil && number > 0 && number < 65536
}

// memberBody decodes a MemberBody and requires its member to be active.
func (state *State) memberBody(proposal Proposal) (MemberBody, error) {
	var body MemberBody
	if err := decodeBody(proposal.Body, &body); err != nil {
		return MemberBody{}, err
	}
	if !state.IsMember(body.MemberID) {
		return MemberBody{}, fmt.Errorf("%w: %q", ErrNotMember, body.MemberID)
	}
	return body, nil
}

func (state *State) remove(nodeID string) {
	delete(state.members, nodeID)
	delete(state.admins, nodeID)
}

// validateSuccession checks a claim of ownership. It is signed by the
// claimant rather than the owner, opens a new epoch, and is justified only by
// a quorum of attestations each signed by its own attestor.
func (state *State) validateSuccession(event Event) (func(), error) {
	if event.Epoch != state.Epoch+1 {
		return nil, ErrWrongEpoch
	}
	successor, ok := state.EligibleSuccessor()
	if !ok || event.SignerID != successor {
		return nil, ErrNotSuccessor
	}
	if err := event.verify(state.members[successor].PublicKey); err != nil {
		return nil, err
	}
	var body SuccessionBody
	if err := decodeBody(event.Payload, &body); err != nil {
		return nil, err
	}
	if body.AbsentOwnerID != state.OwnerID {
		return nil, fmt.Errorf("%w: claim names %q, owner is %q", ErrInvalidAttestation, body.AbsentOwnerID, state.OwnerID)
	}

	seen := make(map[string]bool, len(body.Attestations))
	for _, attestation := range body.Attestations {
		if err := state.validateAttestation(attestation, successor, seen); err != nil {
			return nil, err
		}
		seen[attestation.AttestorID] = true
	}
	if len(seen) < group.RequiredAbsenceAttestations(len(state.members)) {
		return nil, ErrInsufficientAttestations
	}

	return func() {
		delete(state.admins, successor)
		state.OwnerID = successor
		state.Epoch = event.Epoch
	}, nil
}

// VerifyAttestation checks an attestation as the eligible successor's claim
// would: signed by an active member other than the owner and the successor,
// covering a full absence window, and naming a head this log contains. The
// successor uses it to refuse junk before holding an attestation for a claim.
func (state *State) VerifyAttestation(attestation Attestation) error {
	successor, ok := state.EligibleSuccessor()
	if !ok {
		return ErrNotSuccessor
	}
	return state.validateAttestation(attestation, successor, map[string]bool{})
}

func (state *State) validateAttestation(attestation Attestation, claimantID string, seen map[string]bool) error {
	attestorID := attestation.AttestorID
	switch {
	case attestation.GroupID != state.GroupID,
		attestation.Epoch != state.Epoch,
		attestation.AbsentOwnerID != state.OwnerID,
		attestorID == "", attestorID == claimantID, attestorID == state.OwnerID,
		seen[attestorID]:
		return fmt.Errorf("%w: from %q", ErrInvalidAttestation, attestorID)
	}
	attestor, ok := state.members[attestorID]
	if !ok {
		return fmt.Errorf("%w: %q is not a member", ErrInvalidAttestation, attestorID)
	}
	if err := attestation.verify(attestor.PublicKey); err != nil {
		return fmt.Errorf("%w: from %q", err, attestorID)
	}
	// The absence window is the attestor's own judgment, made on its own
	// clock and signed. No receiver's clock is consulted.
	if attestation.UnreachableSince.IsZero() ||
		attestation.IssuedAt.Sub(attestation.UnreachableSince) < group.OwnerSuccessionTimeout {
		return fmt.Errorf("%w: %q has not observed a full absence window", ErrInvalidAttestation, attestorID)
	}
	// The claim must build on everything the attestor had applied.
	if hash, ok := state.hashAt(attestation.Head.Sequence); !ok || hash != attestation.Head.Hash {
		return fmt.Errorf("%w: %q attested a head this log does not contain", ErrInvalidAttestation, attestorID)
	}
	return nil
}
