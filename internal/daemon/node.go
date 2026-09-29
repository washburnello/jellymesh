// Package daemon runs a Jellymesh node: it holds the node's identity and
// durable state, serves the federation listener and the local admin API, and
// runs the heartbeat that keeps the group log in step with the other members.
//
// A node belongs to at most one group in this release (conformance.md
// assumption A-7). Founding or joining a second group is refused.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync"
	"time"

	"jellymesh/internal/audit"
	"jellymesh/internal/catalog"
	"jellymesh/internal/catalogsync"
	"jellymesh/internal/config"
	"jellymesh/internal/enrollment"
	"jellymesh/internal/federation"
	groupwatch "jellymesh/internal/group"
	"jellymesh/internal/grouplog"
	"jellymesh/internal/jellyfin"
	"jellymesh/internal/materialize"
	"jellymesh/internal/membership"
	"jellymesh/internal/node"
	"jellymesh/internal/policy"
	"jellymesh/internal/relay"
	"jellymesh/internal/replication"
	"jellymesh/internal/sourcecatalog"
	"jellymesh/internal/store"
	"jellymesh/internal/syncpolicy"
)

var (
	ErrNoGroup      = errors.New("this node is not in a group")
	ErrAlreadyGroup = errors.New("this node already belongs to a group")
	ErrUnknownPeer  = errors.New("no active member has that node ID")
)

// DatabaseName is the node's database file inside the data directory.
const DatabaseName = "jellymesh.db"

// Node is a running Jellymesh node.
type Node struct {
	cfg      config.Config
	nodeID   string
	identity *node.Identity
	database *store.DB
	logs     *store.GroupLogRepository
	peers    *store.PeerRepository
	policies *store.PolicyRepository
	queue    *store.ProposalQueue
	audit    *audit.Log
	server   *replication.Server
	client   *replication.Client
	logger   *log.Logger
	watches  *store.OwnerWatchRepository
	redactor *audit.Redactor

	// The source side exists only when a Jellyfin service user is
	// configured; without one the node consumes but cannot publish.
	jellyfin *jellyfin.Client
	source   *sourcecatalog.Catalog
	remote   *store.RemoteCatalogRepository
	syncs    *store.SyncRepository

	materialized *store.MaterializedRepository
	materializer *materialize.Materializer
	heads        *relay.HeadCache

	mutex sync.Mutex
	clock func() time.Time
	group *groupRuntime
}

type groupRuntime struct {
	id      string
	group   *membership.Group
	inviter *enrollment.Inviter

	catalogServer *catalogsync.Server
	destination   *catalogsync.Destination

	// watch is this node's view of the owner's reachability, and
	// attestations are those other members have sent this node while it is
	// the eligible successor, keyed by attestor, for the epoch they name.
	succession   sync.Mutex
	watch        *groupwatch.Watch
	attestations map[string]grouplog.Attestation
}

// now reads the node's clock, which tests may replace with SetClock.
func (n *Node) now() time.Time {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if n.clock == nil {
		return time.Now()
	}
	return n.clock()
}

// SetClock replaces the node's clock, so that windows measured in days can be
// exercised without waiting for them.
func (n *Node) SetClock(clock func() time.Time) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.clock = clock
}

// Open loads or creates the node's identity and state under cfg.
func Open(ctx context.Context, cfg config.Config, logger *log.Logger) (*Node, error) {
	if logger == nil {
		logger = log.Default()
	}
	hostname, _, err := net.SplitHostPort(cfg.PublicAddress())
	if err != nil || hostname == "" {
		hostname = cfg.NodeName
	}
	identity, err := node.LoadOrCreate(cfg.NodeKeyPath, cfg.NodeCertPath, hostname)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	database, err := store.Open(filepath.Join(cfg.DataDirectory, DatabaseName))
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg: cfg, identity: identity, database: database,
		logs: store.NewGroupLogRepository(database), peers: store.NewPeerRepository(database),
		policies: store.NewPolicyRepository(database), queue: store.NewProposalQueue(database),
		server: replication.NewServer(identity), logger: logger,
		watches: store.NewOwnerWatchRepository(database),
	}
	n.server.ReceiveAttestations(n.receiveAttestation)
	n.redactor = &audit.Redactor{}
	n.redactor.Register(cfg.JellyfinPassword)
	n.audit = &audit.Log{Sink: store.NewAuditRepository(database), Redactor: n.redactor}
	n.remote = store.NewRemoteCatalogRepository(database, catalog.DefaultDeletionGracePeriod)
	n.syncs = store.NewSyncRepository(database)
	n.peers.SetAudit(n.audit)
	n.client = replication.NewClient(identity, n.server)

	if n.nodeID, err = n.logs.NodeID(ctx); err != nil {
		database.Close()
		return nil, err
	}
	if cfg.JellyfinUser != "" {
		n.jellyfin = jellyfin.New(cfg.JellyfinBaseURL, cfg.JellyfinUser, cfg.JellyfinPassword, n.nodeID)
		n.source = sourcecatalog.New(n.jellyfin, store.NewSourceCatalogRepository(database), cfg.ProtectedLibraries, n.audit)
	}
	n.materialized = store.NewMaterializedRepository(database)
	if n.materializer, err = materialize.New(cfg.GeneratedRootPath, cfg.RelayURL, n.materialized, store.NewWorkPinRepository(database), sourceAccess{n}); err != nil {
		database.Close()
		return nil, err
	}
	if cfg.HeadCacheBytes >= relay.DefaultHeadSize {
		if n.heads, err = relay.NewHeadCache(filepath.Join(cfg.DataDirectory, "cache", "heads"), relay.DefaultHeadSize, cfg.HeadCacheBytes); err != nil {
			database.Close()
			return nil, err
		}
		n.materializer.OnRemove = n.heads.Remove
	}
	groupIDs, err := n.logs.ListGroupIDs(ctx)
	if err != nil {
		database.Close()
		return nil, err
	}
	if len(groupIDs) > 1 {
		database.Close()
		return nil, fmt.Errorf("%w: the database holds %d groups", ErrAlreadyGroup, len(groupIDs))
	}
	if len(groupIDs) == 1 {
		group, err := membership.Open(ctx, n.logs, n.peers, groupIDs[0])
		if err != nil {
			database.Close()
			return nil, err
		}
		if err := n.attach(ctx, groupIDs[0], group); err != nil {
			database.Close()
			return nil, err
		}
	}
	return n, nil
}

// Close releases the node's database.
func (n *Node) Close() error { return n.database.Close() }

// NodeID is this node's stable identifier.
func (n *Node) NodeID() string { return n.nodeID }

// Identity is this node's key and certificate.
func (n *Node) Identity() *node.Identity { return n.identity }

// FederationHandler is the handler for the public federation listener, to be
// served with federation.TLSConfig.
func (n *Node) FederationHandler() *federationRoutes { return &federationRoutes{node: n} }

func (n *Node) attach(ctx context.Context, groupID string, group *membership.Group) error {
	state, err := n.policies.Load(ctx, groupID)
	if err != nil {
		return err
	}
	group.SetAudit(n.audit)
	inviter := enrollment.NewInviter(n.nodeID, n.identity, n.cfg.PublicAddress(), group, groupID, state, n.policies)
	inviter.SetAudit(n.audit)
	watch, found, err := n.watches.Load(ctx, groupID)
	if err != nil {
		return err
	}
	if !found {
		var ownerID string
		_ = group.View(func(state *grouplog.State) { ownerID = state.OwnerID })
		if watch, err = groupwatch.NewWatch(groupID, ownerID); err != nil {
			return err
		}
	}
	n.server.Add(groupID, group)
	n.mutex.Lock()
	n.group = &groupRuntime{
		id: groupID, group: group, inviter: inviter, watch: watch, attestations: map[string]grouplog.Attestation{},
		catalogServer: n.newCatalogServer(group, groupID),
		destination:   catalogsync.NewDestination(n.remote, n.syncs, n.client, groupID, n.audit),
	}
	n.mutex.Unlock()
	return nil
}

// receiveAttestation keeps an attestation sent to this node as eligible
// successor. The replication server has already verified it against the log.
func (n *Node) receiveAttestation(groupID string, attestation grouplog.Attestation) error {
	runtime, err := n.current()
	if err != nil || runtime.id != groupID {
		return ErrNoGroup
	}
	var successor string
	_ = runtime.group.View(func(state *grouplog.State) { successor, _ = state.EligibleSuccessor() })
	if successor != n.nodeID {
		return errors.New("this node is not the eligible successor")
	}
	runtime.succession.Lock()
	defer runtime.succession.Unlock()
	runtime.attestations[attestation.AttestorID] = attestation
	return nil
}

func (n *Node) current() (*groupRuntime, error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	if n.group == nil {
		return nil, ErrNoGroup
	}
	return n.group, nil
}

// Found creates a group owned by this node.
func (n *Node) Found(ctx context.Context, groupID string) error {
	if _, err := n.current(); err == nil {
		return ErrAlreadyGroup
	}
	group, err := membership.Found(ctx, n.logs, n.peers, n.identity, groupID, n.nodeID, n.cfg.NodeName, n.cfg.PublicAddress(), n.now())
	if err != nil {
		return err
	}
	return n.attach(ctx, groupID, group)
}

// Invite issues an invitation from this node.
func (n *Node) Invite(ctx context.Context, validFor time.Duration) (enrollment.Token, error) {
	runtime, err := n.current()
	if err != nil {
		return enrollment.Token{}, err
	}
	return runtime.inviter.Invite(ctx, n.now().Add(validFor))
}

// Redeem presents an invitation to its inviter as this node.
func (n *Node) Redeem(ctx context.Context, token enrollment.Token, libraries []policy.Library) (enrollment.Redemption, error) {
	if _, err := n.current(); err == nil {
		return enrollment.Redemption{}, ErrAlreadyGroup
	}
	// By default a node offers what it actually publishes, which is what the
	// admission rule is about.
	if len(libraries) == 0 {
		published, err := n.publishedLibraries(ctx)
		if err != nil {
			return enrollment.Redemption{}, err
		}
		if len(published) == 0 {
			return enrollment.Redemption{}, errLibrariesRequired
		}
		libraries = published
	}
	return enrollment.Redeem(ctx, n.identity, token, n.nodeID, n.cfg.NodeName, n.cfg.PublicAddress(), libraries)
}

// CompleteJoin checks whether this node's request has been decided and, once
// it is admitted, downloads and adopts the group log.
func (n *Node) CompleteJoin(ctx context.Context, inviter replication.Peer, groupID string, genesis grouplog.Hash) (string, error) {
	if _, err := n.current(); err == nil {
		return "", ErrAlreadyGroup
	}
	status, err := enrollment.Status(ctx, n.identity, inviter)
	if err != nil {
		return "", err
	}
	if status.Status != string(policy.InvitationAdmitted) {
		return status.Status, nil
	}
	group, err := enrollment.Join(ctx, n.identity, inviter, groupID, genesis, n.logs, n.peers)
	if err != nil {
		return "", err
	}
	if err := n.attach(ctx, groupID, group); err != nil {
		return "", err
	}
	return status.Status, nil
}

// Outcome of a proposal made through the node.
type Outcome struct {
	Sequenced bool
	Queued    bool
	Event     grouplog.Event
}

// Propose signs a decision as this node and gets it into the log: directly if
// this node is the owner, by submission to the owner otherwise, and by
// queueing it when the owner cannot be reached. A proposal the rules refuse
// is reported rather than queued.
func (n *Node) Propose(ctx context.Context, kind grouplog.Kind, body any) (Outcome, error) {
	runtime, err := n.current()
	if err != nil {
		return Outcome{}, err
	}
	proposal, err := grouplog.NewProposal(n.identity, runtime.id, n.nodeID, kind, body, n.now())
	if err != nil {
		return Outcome{}, err
	}
	event, err := n.submit(ctx, runtime, proposal)
	switch {
	case err == nil:
		_ = runtime.inviter.Reconcile(ctx)
		return Outcome{Sequenced: true, Event: event}, nil
	case isTransient(err):
		if err := n.queue.Enqueue(ctx, proposal); err != nil {
			return Outcome{}, err
		}
		_ = n.audit.Record(ctx, n.nodeID, "proposal.queued", runtime.id, map[string]string{"group_id": runtime.id, "kind": string(kind)})
		return Outcome{Queued: true}, nil
	default:
		return Outcome{}, err
	}
}

// submit sequences locally when this node owns the group, and otherwise sends
// the proposal to the owner's node.
func (n *Node) submit(ctx context.Context, runtime *groupRuntime, proposal grouplog.Proposal) (grouplog.Event, error) {
	var owner grouplog.Member
	var isOwner bool
	_ = runtime.group.View(func(state *grouplog.State) {
		owner, _ = state.Member(state.OwnerID)
		isOwner = state.IsOwner(n.nodeID)
	})
	if isOwner {
		return runtime.group.Sequence(ctx, n.identity, proposal, n.now())
	}
	event, err := n.client.Submit(ctx, replication.Peer{Address: owner.PublicHostname, Fingerprint: owner.Fingerprint}, runtime.id, proposal)
	if err != nil {
		return grouplog.Event{}, err
	}
	// Apply it here too rather than waiting for the next heartbeat.
	if _, err := runtime.group.Receive(ctx, event); err != nil && !errors.Is(err, grouplog.ErrGap) {
		n.logger.Printf("apply own sequenced event: %v", err)
	}
	return event, nil
}

// isTransient reports whether a failed submission should be retried later:
// the owner was unreachable, or answered from a node that is no longer owner,
// or this node is held after a restore.
func isTransient(err error) bool {
	return errors.Is(err, replication.ErrPeerUnreachable) || errors.Is(err, replication.ErrNotOwner) ||
		errors.Is(err, membership.ErrSequencingHeld)
}

// flushProposals submits queued proposals, oldest first, stopping at the
// first that still cannot reach the owner so that order is kept.
func (n *Node) flushProposals(ctx context.Context, runtime *groupRuntime) {
	pending, err := n.queue.Pending(ctx, runtime.id)
	if err != nil {
		n.logger.Printf("read proposal queue: %v", err)
		return
	}
	for _, proposal := range pending {
		_, err := n.submit(ctx, runtime, proposal)
		if isTransient(err) {
			return
		}
		if err != nil {
			_ = n.audit.Record(ctx, n.nodeID, "proposal.refused", runtime.id, map[string]string{
				"group_id": runtime.id, "kind": string(proposal.Kind), "reason": err.Error(),
			})
		}
		if err := n.queue.Remove(ctx, proposal.ID); err != nil {
			n.logger.Printf("dequeue proposal: %v", err)
			return
		}
	}
}

// Requests gathers pending join requests from every member that serves them
// to this node, which is to say every inviter, when this node administers the
// group.
func (n *Node) Requests(ctx context.Context) ([]enrollment.Request, error) {
	runtime, err := n.current()
	if err != nil {
		return nil, err
	}
	requests := runtime.inviter.PendingRequests()
	for _, member := range n.otherMembers(runtime) {
		remote, err := enrollment.Requests(ctx, n.identity, n.server, member, runtime.id)
		if err != nil {
			continue
		}
		requests = append(requests, remote...)
	}
	return requests, nil
}

// Approve signs the admission for a pending request held by inviterID.
func (n *Node) Approve(ctx context.Context, inviterID string, invitationID string) (Outcome, error) {
	request, err := n.findRequest(ctx, inviterID, invitationID)
	if err != nil {
		return Outcome{}, err
	}
	body, err := enrollment.AdmissionFromRequest(request)
	if err != nil {
		return Outcome{}, err
	}
	// Members connect to a node at the address it advertises, so a node
	// that cannot be reached there is refused now rather than failing later
	// (A-16, C-NT-1).
	if err := enrollment.CheckAdvertisedAddress(ctx, n.identity, request.PublicHostname, request.Fingerprint); err != nil {
		return Outcome{}, err
	}
	return n.Propose(ctx, grouplog.KindAdmission, body)
}

// Deny refuses a pending request held by inviterID.
func (n *Node) Deny(ctx context.Context, inviterID string, invitationID string) error {
	runtime, err := n.current()
	if err != nil {
		return err
	}
	if inviterID == n.nodeID {
		return runtime.inviter.DenyLocally(ctx, n.nodeID, invitationID)
	}
	peer, err := n.peer(runtime, inviterID)
	if err != nil {
		return err
	}
	return enrollment.Deny(ctx, n.identity, n.server, peer, runtime.id, invitationID)
}

func (n *Node) findRequest(ctx context.Context, inviterID string, invitationID string) (enrollment.Request, error) {
	runtime, err := n.current()
	if err != nil {
		return enrollment.Request{}, err
	}
	var requests []enrollment.Request
	if inviterID == n.nodeID {
		requests = runtime.inviter.PendingRequests()
	} else {
		peer, err := n.peer(runtime, inviterID)
		if err != nil {
			return enrollment.Request{}, err
		}
		if requests, err = enrollment.Requests(ctx, n.identity, n.server, peer, runtime.id); err != nil {
			return enrollment.Request{}, err
		}
	}
	for _, request := range requests {
		if request.InvitationID == invitationID {
			return request, nil
		}
	}
	return enrollment.Request{}, fmt.Errorf("no pending request %q at %q", invitationID, inviterID)
}

func (n *Node) peer(runtime *groupRuntime, nodeID string) (replication.Peer, error) {
	var peer replication.Peer
	var found bool
	_ = runtime.group.View(func(state *grouplog.State) {
		if member, ok := state.Member(nodeID); ok {
			peer, found = replication.Peer{Address: member.PublicHostname, Fingerprint: member.Fingerprint}, true
		}
	})
	if !found {
		return replication.Peer{}, fmt.Errorf("%w: %q", ErrUnknownPeer, nodeID)
	}
	return peer, nil
}

func (n *Node) otherMembers(runtime *groupRuntime) []replication.Peer {
	var peers []replication.Peer
	_ = runtime.group.View(func(state *grouplog.State) {
		for _, member := range state.Members() {
			if member.NodeID != n.nodeID && member.PublicHostname != "" {
				peers = append(peers, replication.Peer{Address: member.PublicHostname, Fingerprint: member.Fingerprint})
			}
		}
	})
	return peers
}

// SetBlocked blocks or unblocks another node.
func (n *Node) SetBlocked(ctx context.Context, nodeID string, blocked bool) error {
	return n.peers.SetBlocked(ctx, nodeID, blocked)
}

// SyncResult reports one heartbeat.
type SyncResult struct {
	Reached int
	Failed  int
	Applied int
}

// SyncOnce runs one heartbeat: sync the log from every other member, submit
// queued proposals, reconcile invitations, and lift a restore hold once a
// majority of the other members have been reached with nothing newer to
// offer.
func (n *Node) SyncOnce(ctx context.Context) (SyncResult, error) {
	runtime, err := n.current()
	if err != nil {
		return SyncResult{}, err
	}
	var result SyncResult
	var ownerFingerprint node.Fingerprint
	_ = runtime.group.View(func(state *grouplog.State) {
		if owner, ok := state.Member(state.OwnerID); ok {
			ownerFingerprint = owner.Fingerprint
		}
	})
	ownerReached := false
	others := n.otherMembers(runtime)
	for _, peer := range others {
		synced, err := replication.Sync(ctx, n.client, peer, runtime.group, runtime.id, 0)
		if err != nil {
			result.Failed++
			n.logger.Printf("sync from %s: %v", peer.Address, err)
			continue
		}
		result.Reached++
		result.Applied += synced.Applied
		if peer.Fingerprint == ownerFingerprint {
			ownerReached = true
		}
	}
	n.watchOwner(ctx, runtime, ownerReached)
	n.flushProposals(ctx, runtime)
	if err := runtime.inviter.Reconcile(ctx); err != nil {
		n.logger.Printf("reconcile invitations: %v", err)
	}
	if held, err := n.logs.SequencingHeld(ctx); err == nil && held && result.Applied == 0 && result.Reached >= len(others)/2+1 {
		if err := runtime.group.ConfirmCaughtUp(ctx); err != nil {
			n.logger.Printf("release restore hold: %v", err)
		}
	}
	return result, nil
}

// Run serves the federation listener and runs the heartbeat until ctx ends.
func (n *Node) Run(ctx context.Context, federationListener net.Listener, interval time.Duration) error {
	server := newHTTPServer(n.FederationHandler())
	errs := make(chan error, 1)
	go func() {
		errs <- server.Serve(tlsListener(federationListener, federation.TLSConfig(n.identity)))
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	catalogTicker := time.NewTicker(syncpolicy.DefaultCatalogInterval)
	defer catalogTicker.Stop()
	// The first catalog pass comes soon after start rather than an hour in.
	firstCatalog := time.After(time.Minute)
	for {
		select {
		case <-firstCatalog:
			if _, err := n.SyncCatalogOnce(ctx); err != nil {
				n.logger.Printf("catalog: %v", err)
			}
		case <-catalogTicker.C:
			if _, err := n.SyncCatalogOnce(ctx); err != nil {
				n.logger.Printf("catalog: %v", err)
			}
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return server.Shutdown(shutdown)
		case err := <-errs:
			return err
		case <-ticker.C:
			if _, err := n.SyncOnce(ctx); err != nil && !errors.Is(err, ErrNoGroup) {
				n.logger.Printf("heartbeat: %v", err)
			}
		}
	}
}

// watchOwner updates this node's view of the owner's reachability after a
// heartbeat, and acts on it: a member whose absence window has elapsed sends
// its signed attestation to the eligible successor, and the successor claims
// ownership once it holds a quorum of them. Nothing here changes the owner
// directly; a claim is a log event that every member verifies.
func (n *Node) watchOwner(ctx context.Context, runtime *groupRuntime, ownerReached bool) {
	var ownerID, successor string
	var epoch uint64
	var hasAdministrators, isOwner bool
	var members int
	_ = runtime.group.View(func(state *grouplog.State) {
		ownerID, epoch = state.OwnerID, state.Epoch
		successor, hasAdministrators = state.EligibleSuccessor()
		isOwner = state.IsOwner(n.nodeID)
		members = len(state.Members())
	})

	runtime.succession.Lock()
	defer runtime.succession.Unlock()
	watch := runtime.watch
	if watch.OwnerID != ownerID {
		watch.OwnerChanged(ownerID)
		runtime.attestations = map[string]grouplog.Attestation{}
	}
	now := n.now()
	switch {
	case isOwner, ownerReached:
		_ = watch.OwnerAvailable(now)
	default:
		_ = watch.OwnerUnavailable(now, hasAdministrators)
	}
	decision := watch.Advance(now, hasAdministrators)
	if err := n.watches.Save(ctx, watch); err != nil {
		n.logger.Printf("save owner watch: %v", err)
	}
	if decision.Notify {
		_ = n.audit.Record(ctx, "local", "group.owner_unreachable", runtime.id, map[string]string{"group_id": runtime.id, "node_id": ownerID, "status": string(watch.Status)})
	}
	if decision.Dissolve {
		_ = n.audit.Record(ctx, "local", "group.dissolved", runtime.id, map[string]string{"group_id": runtime.id})
		return
	}
	if !decision.ClaimEligible || isOwner {
		return
	}

	if successor == n.nodeID {
		var attestations []grouplog.Attestation
		for _, attestation := range runtime.attestations {
			if attestation.Epoch == epoch && attestation.AbsentOwnerID == ownerID {
				attestations = append(attestations, attestation)
			}
		}
		if len(attestations) < groupwatch.RequiredAbsenceAttestations(members) {
			return
		}
		if _, err := runtime.group.Claim(ctx, n.identity, n.nodeID, attestations, now); err != nil {
			n.logger.Printf("claim ownership: %v", err)
			return
		}
		watch.OwnerChanged(n.nodeID)
		runtime.attestations = map[string]grouplog.Attestation{}
		_ = n.watches.Save(ctx, watch)
		return
	}

	var attestation grouplog.Attestation
	var attestErr error
	_ = runtime.group.View(func(state *grouplog.State) {
		attestation, attestErr = grouplog.Attest(n.identity, n.nodeID, state, watch.OwnerUnavailableSince, now)
	})
	if attestErr != nil {
		n.logger.Printf("attest: %v", attestErr)
		return
	}
	peer, err := n.peer(runtime, successor)
	if err != nil {
		return
	}
	if err := n.client.SendAttestation(ctx, peer, runtime.id, attestation); err != nil {
		n.logger.Printf("send attestation to %s: %v", successor, err)
		return
	}
	_ = n.audit.Record(ctx, n.nodeID, "group.absence_attested", runtime.id, map[string]string{"group_id": runtime.id, "node_id": ownerID})
}
