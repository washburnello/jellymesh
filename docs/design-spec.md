# Jellymesh Design Specification

Status: Draft architecture specification
Date: 2026-09-23
Target Jellyfin baseline: 10.11.x, starting with Cedar’s 10.11.11.0 deployment

## 1. Summary

Jellymesh makes media owned by trusted friends appear inside a normal Jellyfin experience without copying the media or requiring custom Jellyfin clients.

Each home server runs a Jellymesh Service. The Jellymesh Service has two roles:

- Publisher: exposes explicitly shared Jellyfin libraries to approved peer servers.
- Consumer: synchronizes approved peer catalogs into generated Jellyfin media references under a local `Friends` library.

The local Jellyfin server remains responsible for users, local permissions, normal library behavior, playback sessions, and local watch progress. The Jellymesh Service remains responsible for federation identity, library publication, destination opt-out decisions, catalog synchronization, source health, generated media references, and source-authenticated media relay.

The initial design favors a relay-backed `.strm`/metadata materializer because it gives normal Jellyfin clients the best chance of working without a Jellyfin fork or client changes. The implementation must validate this choice before depending on it.

## 2. Goals

- Use stock Jellyfin clients and Jellyfin Web.
- Validate Jellyfin Web, Android, Android TV, iOS/Swiftfin, and Roku TV on day one; other clients are best-effort until tested.
- Share movies, television series, and music between independent home servers.
- Keep `Family Movies` and other private libraries private by default.
- Publish approved source libraries into a group pool, with destinations auto-accepting published libraries and optionally opting out of individual source libraries.
- Admit only server-owning members; a server must publish at least one non-empty library before joining the group.
- Require a group owner and an explicitly assigned administrator pool; owner succession comes from that pool after 15 days.
- Show one logical card with multiple selectable source versions where Jellyfin supports it.
- Show the owning server’s friendly name in the native version information.
- Integrate remote media into the destination’s existing library for each collection type, so local and remote items can participate in the same native collections and version relationships.
- Keep collections and playlists destination-local; never replicate or broadcast source collections or playlists.
- Keep watched state, resume positions, favorites, ratings, and playback history local to the viewing user’s server.
- Work across public HTTPS without requiring Tailscale.
- Run as a normal self-hosted home service with durable state, retries, backups, and observable health.
- Keep Jellymesh focused on federated library access; local Jellyfin owns local users, permissions, and household policy.
- Make approved remote libraries available to group members, while allowing each destination to opt out of any exact source library.
- Avoid media copying and shared-filesystem requirements.

## 3. Non-goals for the first release

- No central cloud account or availability dependency.
- No shared global administrator credential.
- No cross-server passwords, cookies, or user tokens.
- No cross-server watch-state synchronization.
- No cross-server user accounts, groups, or permission synchronization.
- No receive-only group members or viewer-only Jellymesh servers.
- No custom Jellyfin Web client.
- No replication or broadcast of source collections, playlists, or other user-created metadata objects.
- No guaranteed greyed-out provider state in stock clients.
- No guarantee that a circular source avatar can appear in every official client.
- No photo, book, home-video, playlist, collection, or Live TV federation.
- No requirement that a friend’s raw Jellyfin administration port is exposed publicly.
- No direct client-to-provider media path in the initial relay-backed design.

## 4. Terminology

### Node

One Jellymesh deployment associated with one Jellyfin server. A node has a stable random node ID, a human-readable name, a public HTTPS endpoint, and its own signing identity.

### Jellymesh Service

The Jellymesh Service is the Jellymesh process that performs federation work. It can publish, consume, or both.

### Library

A Jellyfin library is an admin-configured scanned source with its own path, collection type, permissions, and metadata behavior. A library is the unit of sharing and authorization.

Examples on Cedar include:

- Movies
- TV Shows
- Music
- Family Movies

### Collection type

The broad Jellyfin library classification, such as Movies, TV Shows, Music, Books, Photos, or Mixed Content. A collection type is not precise enough for access control because several separate libraries can have the same collection type.

### Work

The logical movie, series, album, or track shown as one card to a user. A work may have one or more source media versions.

### Media source

A particular playable copy or version owned by one provider node. A media source belongs to one provider library and carries the source’s own quality, edition, codec, and path information.

## 5. Confirmed product decisions

- Stock Jellyfin clients and UI are the target experience.
- Every server acts as an independent hub in a full mesh.
- Public HTTPS is the target transport.
- Exact source-library publication and destination opt-out decisions are required.
- Movies, television, and music are in scope.
- Cedar’s `Family Movies` must never be exposed remotely.
- One logical card with multiple versions is desired.
- The owning server’s friendly name should be visible.
- Watch progress remains local to the local user’s server.
- Heartbeats and offline awareness are desired, but provider-offline visual state is a stretch goal.
- Normal people should be able to run the service on a home server without understanding federation internals.

## 6. Architecture

```text
Official Jellyfin client
          |
          | HTTPS to local Jellyfin
          v
Local Jellyfin
          |
          | local filesystem/API integration
          v
Local Jellymesh Service
          |
          | authenticated public HTTPS to approved peers
          +-----------------------+
          |                       |
          v                       v
Peer Jellymesh Service       Peer Jellymesh Service
          |                       |
          v                       v
Peer Jellyfin             Peer Jellyfin
```

### Local Jellyfin

The local Jellyfin server remains authoritative for:

- local users and passwords;
- local sessions;
- local library permissions;
- local watched state and resume points;
- local favorites, ratings, and play history;
- native playback behavior and client compatibility.

### Jellymesh Service

The Jellymesh Service is authoritative for:

- node identity and public key material;
- pairwise peer relationships;
- source-library publication and destination opt-out decisions;
- catalog snapshots and sync cursors;
- generated `.strm` and metadata artifacts;
- provider health and retry state;
- source-authenticated relay behavior;
- audit and operational telemetry.

The Jellymesh Service should not write directly to Jellyfin’s SQLite database. It should use supported Jellyfin APIs and a generated media root that Jellyfin scans as a normal library.

## 7. Library privacy, publication, and opt-out

Every source library receives a federation classification:

```text
private
published
```

All libraries begin private. A source server must explicitly publish each library into the group pool during onboarding or later administration. Publication makes a library available to group members by default; a destination may opt out of any exact source library.

A destination policy is recorded per source library and contains at least:

```text
group_id
source_node_id
source_library_id
opted_out: true | false
updated_at
```

A source publication does not imply transitive trust. Cedar publishing `Movies` to the group does not give Cedar access to another node. A destination that has not opted out receives future items added to the published library automatically.

### Cedar protection rule

Cedar’s `Family Movies` library is a hard-denied private library. The implementation must enforce this rule at every relevant layer:

1. publication configuration refuses to select the protected library;
2. catalog projection omits the protected library;
3. receiver sync refuses to materialize it;
4. stream and subtitle endpoints reject it;
5. diagnostics and health endpoints do not enumerate it;
6. name-only matching is not sufficient; stable library identity and a normalized path fingerprint are required;
7. a renamed or recreated library fails closed and requires explicit reclassification.

The private library may remain available to local Cedar users, but it must not appear in any remote catalog or playback surface.

### Destination library policy

A source publication is the maximum eligibility the provider is willing to offer. A destination is automatically eligible when the library is published and can opt out of any exact source library.

Effective federated visibility is:

```text
source publication
AND NOT destination opt-out
AND local Jellyfin library access
```

Jellymesh does not perform item-level rating, genre, tag, or per-user filtering. Local Jellyfin users, local permissions, parental controls, and household policy remain the responsibility of the local Jellyfin server.

A destination opt-out must not leak through catalog, search, artwork, guessed item IDs, subtitle requests, or direct stream paths. The local operator interface should show a simple peer directory with:

- friendly server name and health;
- last seen and last successful catalog sync;
- source library names, collection types, item counts, and approximate sizes;
- current published-library pool;
- destination opt-out decisions;
- block state;
- sync and playback errors.

The Jellymesh settings page is the local administrator surface. It includes published-library controls, per-source opt-outs, the remote-transcode limit, invitation and role management, and sync health. Group members can see aggregate peer health and sync status, while detailed failure history, affected item counts, retry state, and sanitized diagnostics remain local-admin-only. Group-wide defaults are visible to members but editable only by owners and administrators; they apply automatically to reachable nodes without an explicit local override, while local overrides win and changes are auditable. This interface is not part of the end-user Jellyfin experience.

### Admission rule

Jellymesh group membership is for server owners, not viewers. A joining server must:

1. run its own Jellyfin server and Jellymesh Service;
2. generate its own node identity;
3. explicitly publish at least one non-empty source library;
4. accept the group’s current admission policy;
5. complete the owner/admin enrollment flow.

A server with no published library is not admitted. People who only want to consume media should be given a local user account on a friend’s Jellyfin server; Jellymesh does not create viewer-only federated memberships.

## 8. Peer identity and enrollment

Each node creates a stable node UUID and a long-term Ed25519 signing key. A hostname, URL, Jellyfin API key, or shared database is not a node identity.

The TLS certificate proves control of a public hostname. It does not prove that a peer is approved by Jellymesh. Peer authorization uses a separate node identity and pairwise trust record.

### Transport authentication

Peer-to-peer requests are authenticated with **mutual TLS**, using the node's long-term Ed25519 key as the client certificate key. TLS 1.3 supports Ed25519 certificates natively.

The rules are:

1. Every Jellymesh-to-Jellymesh request presents a client certificate.
2. Authorization matches the presented certificate's public key fingerprint against the pairwise trust record. It does not match on hostname, address, or certificate chain.
3. A valid certificate from an unknown key is rejected. A known key arriving from a new hostname or address is accepted, because the node identity is authoritative and the hostname is not.
4. A node terminates its own TLS on its own listener. It does not rely on a fronting proxy to perform or forward client-certificate verification.
5. A deployment that must place Jellymesh behind another component uses TCP or SNI passthrough. TLS termination by a proxy is unsupported, because a forwarded client-certificate header is spoofable if the proxy is misconfigured.

The rationale for mutual TLS over signed request envelopes or pairwise bearer tokens:

- **It adds no clock dependence.** Replay protection comes from the TLS session rather than from a nonce cache and a timestamp-skew window. Membership, invitation, retention, and dissolution timers already depend on local wall-clock time, and the transport should not deepen that exposure.
- **Node identity and transport identity are the same object**, so there is no second binding between a request and a key that can be implemented incorrectly.
- **Streaming inherits authentication.** The media path is range-heavy and includes aborted open-ended reads, and per-connection authentication fits that traffic shape without per-request work.
- **It introduces no additional bearer secret.** Anything bearer-shaped is a liability given that the local Jellyfin surface already discloses media source paths to ordinary local users.

The public Jellymesh listener is separate from the local relay listener. The relay listener is never exposed publicly and never accepts peer traffic.

#### Key compromise and recovery

There is no key rotation protocol, and this is deliberate.

The obvious design is a rotation event signed by the outgoing key. That works
for planned replacement and fails in the only case that matters. An attacker
holding a stolen key can sign a rotation naming their own key as the
replacement, quite possibly before the owner notices, and the group then trusts
the attacker and locks out the legitimate node. The recovery mechanism becomes
the attack.

Recovery is therefore by **re-enrollment**. A node whose key is compromised is
treated as a new node: the existing member is ejected through the normal signed
revocation, the operator generates a fresh identity, and that identity is
admitted through a new invitation and a fresh owner or administrator approval.
The node's fingerprint changes, so it is a different peer by construction and
no stale grant can survive.

The cost is real and accepted: the node loses continuity, peers resynchronize
its catalog from scratch, and its destination opt-out decisions about it are
re-evaluated. For a group of roughly twenty people who can contact each other
out of band, obtaining a new invitation is a reasonable recovery path, and it
removes an entire class of failure rather than building a delicate mechanism to
manage it.

A printed recovery key, generated at install and held offline, is the natural
later addition if operational experience shows re-enrollment is too disruptive.
It is not required for the first release.

Two consequences follow for the implementation. Ejection must revoke trust at
the transport layer and not only in group membership, or a compromised key
would still complete a mutual-TLS handshake. And an operator runbook must
document the sequence, because recovery is a procedure rather than a feature.

Enrollment is owner/admin-approved and member-sponsored:

1. Any active group member creates a short-lived, one-use invitation from the Jellymesh settings page.
2. The invited node generates its own key pair.
3. The invited node redeems the invitation and submits its identity fingerprint.
4. The owner and administrators receive a notification identifying the inviter, invitee, and fingerprint.
5. An owner or admin approves or denies the request.
6. Before approval, the invited node must publish at least one non-empty library into its candidate publication set.
7. Approval produces a signed membership admission event.
8. The invited node establishes pairwise trust links with approved group members.
9. The invitation is consumed and cannot be reused.
10. A redeemed request remains pending if all owner/admin decision-makers are unavailable; it is not automatically denied. The original invitation expiration applies until redemption, not after it.

A permanent shared group key may be used only as a future invitation capability if the product requires that UX. It must never authorize catalog access, playback, membership changes, or stream URLs.

Trust and authorization are separate:

- trust says two nodes may communicate;
- a publication says a source library is eligible for group members;
- an opt-out decision says one destination node chose to hide that source library;
- membership says a node participates in the group;
- a block says two nodes remain group members but have no media-sharing relationship.

Group coordination is separate from the media data path. The creator of a group becomes its owner. The owner may explicitly promote members to administrators. Admins can approve invitations, eject ordinary members, and perform other group operations, but they cannot eject other admins or remove the owner.

If the owner is unavailable for 15 days, the oldest administrator (earliest promotion; member ID breaks exact timestamp ties) becomes owner. Administrators are not randomly or automatically appointed; they are assigned by the owner.

Succession is **claimed, not applied on a timer**. Each node observes the owner's reachability independently and on its own clock, a blocked peer receives no heartbeats from the peer it blocked, and a partitioned node receives none from anyone. If every node promoted locally when its own timer expired, two nodes could install different owners and then sign conflicting admissions and revocations, with no rule to resolve the conflict.

The absence window therefore only makes a claim *eligible*. To install a new owner:

1. the claimant must be the eligible successor under the rule above;
2. the absence window must have elapsed;
3. the claimant must present absence attestations from a quorum of distinct other members.

An attestation is one member's statement that it has also been unable to reach the owner, since at least as far back as the claimant's own absence window began. The quorum is a strict majority of the members other than the absent owner, with the claimant's own observation counting as one of those voices. A group of three therefore needs one attestation, five needs two, and twenty needs nine. A two-member group needs none, which is correct rather than lax: with only the owner and one other member there is no second observer who could disagree.

Attestations from the claimant itself, from the absent owner, from non-members, and duplicates from one member are all rejected, so a single node cannot manufacture a quorum.

If no owner or admin remains, the group enters a timeout-based dissolution state. Members receive an immediate notification and then notifications every 5 days. The group dissolves after 15 days without an available owner or admin. Recovery before the deadline restores normal operation; recovery after dissolution requires a new group.

The owner and admins may sign invitations, memberships, policy updates, and revocations, but they cannot own, inspect, or impersonate member media. A node can leave or revoke its own participation, and any member can block a specific peer locally. A local `Block Server` action is modeled as a symmetric pairwise media cut: the two nodes remain group members, but neither side exchanges catalogs or media with the other. Unblocking restores normal pool behavior while preserving explicit opt-outs.

A block is not a global membership mutation and must not affect unrelated peers. Because transport trust already refuses a blocked peer, a block also stops the two nodes exchanging the group log directly. Each still receives the log through the other members. A node that leaves or blocks a peer may still remain visible in other nodes’ group directories according to each node’s local policy.

An owner or admin may eject an ordinary member by publishing a signed membership revocation. Ejection removes the member from future group participation, invitations, catalog publication, and new playback authorization. It cannot erase media, metadata, or other data already downloaded by the member. The owner cannot be ejected. A rejected member must receive a new invitation and fresh approval to rejoin; an old grant or membership record is never silently reinstated.

An ejected server is untrusted and is not responsible for cleanup. The revocation is signed, sequenced, and propagated to the other group members. Every receiving node independently denies future access, removes its own generated Jellymesh artifacts and caches, triggers a local Jellyfin rescan, and verifies that the rejected server’s content is absent. The rejected server cannot restore access by withholding a catalog response, tombstone, or cleanup operation.

For a group of approximately twenty nodes, the owner and administrators form a bounded control plane while media and playback remain pairwise. Each node maintains its own peer directory, pairwise trust records, library publications, opt-out decisions, blocks, and health state. Full mesh connectivity is acceptable, but it must be bounded by pagination, incremental sync, rate limits, and backoff.

### Group state and event replication

Status: assumed (conformance.md assumption A-5), 2026-09-26. The log is
implemented in `internal/grouplog`, stored by `store.GroupLogRepository`, and
held by `membership.Group`, which derives transport trust from it.
`internal/policy` consults the roster rather than keeping one, and
`internal/group` is reduced to this node's own observation of the owner's
availability. `internal/replication` serves and fetches the log over mutual
TLS. Enrollment against the log follows.

Everything above describes what the group decides; this describes how every member comes to
agree on it. Without it, signed events were verifiable but had no order: two
administrators could claim the same sequence number, an event that arrived
after a later one was dropped permanently, and an admission could only be
applied by the node that had seen the invitation.

#### What is replicated and what is not

State falls into two classes, and they are stored and changed differently.

**Group state** is identical on every member, because every member derives it
from the same log:

- the owner, and the current ownership epoch;
- the administrator set, with promotion times, which succession depends on;
- the roster: each member's node ID bound to its key fingerprint, friendly name,
  and public hostname;
- ejected nodes, so a stale grant is never reinstated;
- group-wide defaults editable by owners and administrators.

**Node-local state** belongs to one node, which decides it alone and never
replicates it:

- its own library publications. The source is authoritative, and serves them to
  each peer directly over mutual TLS;
- its destination opt-outs and its blocks;
- health, sync cursors, retention, and history;
- the invitations it issued, including their secrets.

Transport trust is **derived, not stored as a separate decision**. A peer is
trusted when its fingerprint belongs to an active member in the group state and
this node has not blocked it. Admitting a member therefore trusts it on every
node at once, and ejecting one untrusts it everywhere. There is no second flag
to fall out of step with the roster. The one exception is the enrollment
endpoint, described below.

#### The log

Group state is the result of replaying an append-only log of signed events.
Every event carries:

```text
group_id
epoch         ownership term; increments only on succession
sequence      1, 2, 3, ... with no gaps
prev_hash     SHA-256 of the previous event's signed bytes
kind          genesis | admission | ejection | leave | promote | demote |
              succession | policy_update
issued_at     informational; ordering never depends on clocks
payload
signature     by the owner of this epoch
```

**The owner is the only sequencer.** Only the owner's node assigns a sequence
number, so two events can never claim the same slot. Administrators do not
append to the log. They sign a *proposal*, for example "approve invitation X"
or "eject member Y", and send it to the owner. The owner checks it against the
current state and appends an event that embeds the signed proposal. Every
receiver checks both signatures: the owner's for the ordering, and the
proposer's for the authority. It then re-applies the same role rules, so an
owner cannot fabricate an administrator's decision, and an administrator's
decision cannot exceed an administrator's powers.

The cost is that when the owner's node is offline, proposals queue at the
proposer rather than taking effect. That is accepted for a group of roughly
twenty households. An absence long enough to matter is what succession is for.

**The genesis event** (sequence 1) is signed by the creating owner. It records
the group ID and the owner's node ID and fingerprint. Its hash anchors the
group. An invitation carries it, so a joining node can verify every event it
downloads back to a root it received out of band from someone it trusts.

#### Replication

Replication is pull-based over the existing mutual-TLS connections:

1. Every heartbeat exchanges each side's log head (`epoch`, `sequence`, hash),
   at `GET /jellymesh/v1/groups/{group}/log/head`.
2. A node that is behind requests the missing range from that peer, or from any
   other member, at `GET /jellymesh/v1/groups/{group}/log/events?after=N`, in
   pages of at most 256. Events verify themselves, so a relaying peer cannot
   forge them; it can only withhold them, which asking a different peer
   defeats.
3. Events are applied strictly in order. An event that does not extend the
   current head is held back or refetched. It is never skipped, and a gap never
   causes an event to be dropped.
4. A peer on a later epoch may replace events this node holds from the closed
   one, at a point not known in advance, so the log is refetched from genesis.
   Events already held return as duplicates. For logs of this size that is
   cheaper than searching for the fork.
5. A peer level with this node in the same epoch but with a different head hash
   is asked for the event at its head. Either it is a duplicate, or two events
   claim one slot, which is equivocation and halts the log.
6. A member submits a signed proposal to the owner's node at
   `POST /jellymesh/v1/groups/{group}/proposals`, which sequences it and returns
   the event. A node that is not the owner answers 409, and a proposal the rules
   refuse is answered 422 with the reason, which any member could compute from
   the log anyway. Any member may relay a proposal, since its signature is what
   authorizes it.
7. Each request is authorized against the one group it names: the connection's
   key must belong to a member of that group. Anything else is answered as not
   found, so a node serving several groups reveals nothing about the others.

A new member downloads the whole log from genesis. For a group of this size
the log stays small, so compaction is deferred.

**Equivocation.** Two different events with the same epoch and sequence, both
correctly signed by that epoch's owner, can only come from a compromised or
misbehaving owner, or from an owner node restored from a stale backup. A node
that sees both stops applying events past that point, keeps both as evidence,
and raises an operator alert. This rule has a consequence for backups: an owner
node restored from backup must sync the log from its peers before it sequences
anything. `internal/backup` enforces this. A restore
places a durable hold on sequencing and on succession claims. The hold is
lifted only when the node confirms it has caught up, so a restored owner
cannot sign a second event for a slot it had already published.

#### Succession and fencing

Succession is a log event, and it opens a new epoch. The node daemon drives
it from the heartbeat. Each beat that fails to reach the owner feeds the
node's owner-absence watch. Once the window has elapsed, a member sends its
attestation to the eligible successor at
`POST /jellymesh/v1/groups/{group}/attestations`, and the successor claims as
soon as it holds a quorum.

1. Each attestation is **signed by its attestor**. It covers the group, the
   epoch, the absent owner, `unreachable_since`, and the attestor's own log head
   at the time it signed.
2. The eligible successor collects a quorum and appends a `succession` event.
   The event carries the new epoch, extends the highest head named by any
   attestation (the claimant must fetch up to it first), embeds the
   attestations, and is signed with the claimant's own key. It is the first
   event of the new epoch.
3. A node that accepts the succession event rejects every event from the old
   epoch that sits beyond the claim's base. **The returning owner is thereby
   fenced.** When it syncs, it finds the succession event and continues as an
   ordinary member, and the new owner may promote it again.
4. A minority of members that still reached the old owner may have applied
   old-epoch events beyond the base. Because group state is a pure replay of the
   log, those members truncate back to the base and replay the new epoch. Any
   proposal that was lost this way is re-proposed.

#### Enrollment against the log

Status: implemented in `internal/enrollment`, with the listener assembled by
`internal/federation`. The short code's contents are assumption A-6.

1. **The invitation.** An invitation is a 64-bit one-time secret plus what the
   joining node needs to authenticate the inviter. The inviter stores only a
   hash of the secret. It has two encodings:
   - the **QR code** carries the inviter's address and full key fingerprint, the
     group ID, the genesis hash, and the secret;
   - the **short code** is 29 characters of Crockford base32, entered together
     with the inviter's address. It carries the first 80 bits of the inviter's
     fingerprint and the secret. Crockford's alphabet leaves out I, L, O and U,
     and parsing ignores case, spaces and dashes.
2. **Redemption.** The invitee connects to the inviter at
   `POST /jellymesh/v1/enroll`, presenting its own node certificate. It accepts
   the connection only if the inviter's key matches the invitation: exactly for
   a QR code, by the 80-bit prefix for a short code. The secret is sent only
   after that check. A server without the inviter's key never receives it,
   because finding a key that matches 80 bits of fingerprint is infeasible.
   - The key the admission will bind is the one in the invitee's certificate.
     The request body cannot name a key, and unknown fields are refused.
   - The body gives the invitee's node ID, name, hostname, and at least one
     library to publish. These are staged as candidates at the inviter, which is
     where the admission rule is enforced.
   - The response gives the group ID and genesis hash. It arrives over the
     pinned connection, which is what makes the genesis trustworthy when the
     invitee started from a short code. For a QR code, a response naming a
     different genesis is refused.
3. **Failures.** A wrong, used, or expired secret gets the same 404, so a
   guesser learns nothing. Redemption failures are limited across all clients,
   to ten in ten minutes, because a guesser can present a fresh key each time.
   While over the limit, every redemption is refused, including a correct one.
   That delays genuine joins during a flood, which is acceptable for an
   endpoint used a few times a year.
4. **The decision.** An owner or administrator fetches the pending requests
   from the inviter (`GET /jellymesh/v1/groups/{group}/requests`). Each request
   carries the invitee's key, fingerprint, and candidate libraries, and the
   approver checks that the key matches the fingerprint. The approver then signs
   an `admission` proposal and submits it to the owner, who sequences it. An
   administrator may instead deny the request at the inviter
   (`POST .../requests/{invitation}/deny`). Both routes answer 404 to anyone who
   is not an owner or administrator.
5. **Applying it.** Every member, including those that never saw the
   invitation, applies the admission from the log alone. From that moment the
   new member's key is trusted everywhere. The inviter marks the invitation
   admitted when it reconciles with the log.
6. **Joining.** The invitee polls `GET /jellymesh/v1/enroll/status`. Once it is
   admitted, it downloads the log from the inviter, now as a member. It anchors
   the log to the genesis hash, and it refuses the log unless the log admits the
   invitee's own key. It checks this before storing anything.

The federation listener's TLS layer accepts any Ed25519 client key, because an
invitee must connect before it is a member. Routing is therefore the
authorization boundary. Only paths under `/jellymesh/v1/enroll` are reachable
by a key outside every roster. Every other route needs a member's key before
its own per-group check runs, so a route added later is private unless it is
deliberately made public.

### Reachability and direct media paths (Phase 5)

Status: decided with the user on 2026-09-29 (conformance.md assumption A-16);
not yet built. The tasks are the GitHub issues under "Direct peer connections".

Members' homes sit behind NAT, sometimes two layers of it (cedar's own path is
Calix gateway, then Google Wifi, then the host, with no access to the outer
router). The decision separates arranging a connection from carrying media.

**Every node advertises a reachable address.** It is part of membership: the
`PublicHostname` in each admission. It is reached over TCP with mutual TLS and
pinned keys, as today, and there are two accepted ways to provide it:

- a router port forward, where the operator controls every NAT layer; or
- Tailscale Funnel in raw-TCP mode (`tailscale funnel --tcp`), which passes
  Jellymesh's TLS through unopened. Funnel's HTTPS mode terminates TLS at
  Tailscale and would break key pinning, so it is not acceptable.

An approver's node dials a joiner's advertised address and checks that the
joiner's key answers there before the admission is signed, so an unreachable
node is caught at the door.

**The advertised address arranges connections; media prefers a direct UDP
path.** Funnel carries every byte through Tailscale's servers, under
bandwidth limits it does not publish, so it suits control traffic but not
film-sized streams. When one node needs media from another:

1. Each learns its outside UDP address (IP and port) from public STUN servers,
   using the same UDP socket its QUIC transport will use.
2. They exchange those addresses, and their LAN addresses for peers on the
   same network, over the TCP connection to the source's advertised address.
   The offers are bound to that mutually authenticated connection and expire
   within seconds.
3. Both send UDP packets to the other's addresses at once. Each router sees
   outgoing traffic and admits the peer's replies: the hole punch. "At once"
   is strict. Linux conntrack NAT, common in home routers, records a peer's
   packet that arrives before its own node has sent anything, then moves
   that node's outgoing packets to another port, so the punch fails (NAT
   lab, M-13). The answer therefore carries a start time half a second
   ahead, by the answering node's clock. The caller converts it to its own
   clock using the clock difference it measures over the request's round
   trip, timed from the request being written to the first byte of the
   answer so that connection setup does not skew it. Both then start within
   well under the one-way delay between homes.
4. A QUIC connection opens over that path, with the same node certificates
   and pinned fingerprints as the TCP transport, and media requests run over
   HTTP/3 on it. The source still authorizes every request; ranges, HEAD,
   cancellation, the per-destination ceiling, and the head cache behave as
   they do over TCP. Keep-alives hold the NAT mappings open.
5. If punching fails, as it can when both routers allocate a new outside
   port per destination, media falls back to the advertised TCP address.

The direction a connection is opened in does not decide the direction media
flows, so a node with a port forward is reachable by UDP without punching,
and a Funnel-only node still serves media directly whenever punching works.
The advertised addresses are known to every member through the roster, so
there is no central matchmaker: each pair arranges its own path.

**As built.** The daemon opens the UDP socket at
`JELLYMESH_DIRECT_LISTEN_ADDR` (default `0.0.0.0:44843`; `off` disables
direct paths) and refreshes its outside address from
`JELLYMESH_STUN_SERVERS` every 15 minutes. It offers that address only if
its NAT keeps one mapping for every destination, plus any addresses in
`JELLYMESH_DIRECT_CANDIDATES`, such as a LAN address for members on the
same network. Playback never waits for a direct path:
- The first media request to a peer goes over TCP and starts an attempt in
  the background.
- Later requests use the direct path once it is open.
- A failed attempt is retried no sooner than 2 minutes later, after
  conntrack has forgotten it.
- A direct request that fails is retried over TCP, and a body that breaks
  part-way is continued over TCP from the byte where it stopped.

`jellymesh status` shows each peer's path. In a container, publish UDP
44843 so that Docker's own NAT does not move the port.

**Security of the UDP surface** (reviewed 2026-09-29, #55, C-NT-7):

| Threat | Mitigation | Residual risk |
|---|---|---|
| Anyone on the internet sends QUIC to the UDP port | A connection attempt is refused before any cryptography unless an accepted offer named its source address. The handshake then requires the offering member's pinned key. `quic-go` limits a server's reply to three times what it has received before validation | Stray packets cost a lookup each; a volumetric flood is a network problem, as for any open port |
| A member uses offers to aim punch traffic at others | An offer is accepted only from its authenticated sender. It names at most 8 ordinary unicast addresses (no loopback, link-local, multicast, or unspecified). Punching sends a 15-byte datagram every 100 ms for at most 5 s. One accepted offer per member per 10 s | A member can make this node send at most about 400 small datagrams per 10 s toward 8 addresses: negligible, and audited |
| A member floods offers | The rate limit above, and each refusal is audited. The TLS listener, the roster, and the block list turn away non-members and blocked members first | None beyond the listener's own load |
| Forged STUN answers set a wrong outside address | Answers are believed only from the server asked, for the transaction sent | An on-path attacker who sees the request can spoil punching; media then falls back to TCP (denial, not compromise) |
| Offers disclose addresses | Offers go only to members, over their authenticated connection, and name the outside address members could see anyway, plus only configured LAN addresses | Members learn a peer's configured LAN address |
| Audit leaks addresses | Direct-path events record node IDs, outcomes, and reasons only | None |
| Replay of an offer | Nonces are remembered for the offer's life plus clock tolerance, and offers ride a TLS connection | None known |

Not in the first cut: relaying signalling through a third member when
neither node is reachable (the requirement above rules that case out),
relaying media through a third member when punching fails at both ends, and
opening router ports automatically by UPnP or NAT-PMP.

## 9. Catalog synchronization

The source node maintains a versioned group catalog and can produce a destination-scoped manifest containing only published libraries that the destination has not opted out of. The source never sends an opted-out library’s metadata to that destination.

Each source item has:

```text
source_node_id
source_library_id
source_item_id
source_item_revision
item_type
parent_source_item_id
provider_ids
normalized_metadata
media_source_descriptors
image_descriptors
tombstone_state
```

The consumer maintains an explicit mapping from:

```text
source_node_id + source_item_id
```

to a stable Jellymesh work ID and generated artifacts. Global IDs must not be derived from mutable URLs.

Synchronization must:

- use pagination and bounded memory;
- process parents before children;
- upsert idempotently;
- reject older revisions;
- use checksums to avoid unnecessary writes;
- preserve known-good artifacts during transient network failure;
- use signed or otherwise authenticated manifests;
- apply explicit tombstones rather than deleting because a heartbeat failed;
- on a confirmed source tombstone, remove generated Jellyfin references immediately, trigger a rescan, and retain Jellymesh metadata/artwork/identity/history in a separate retention area for 7 days by default; purge retention after the grace period if the item does not return;
- never delete files outside the Jellymesh-owned generated root;
- never write partial artifacts visible to Jellyfin.

### Catalog protocol

Status: assumed (conformance.md assumptions A-8 and A-9), 2026-09-27.
This is the Phase 2 design. Materialization and the relay are Phase 3.

#### The source adapter (A-8)

A node reads its own Jellyfin as a **dedicated, non-administrator service
user**, never with an administrator API key. The operator creates that user
and grants it exactly the libraries the node may publish. Two consequences
follow:

- A library the service user cannot see can never be published. The
  protected-library guarantee therefore rests first on Jellyfin's own
  permission system, and only second on Jellymesh's checks (plan-review B3).
- Compromising Jellymesh does not grant Jellyfin administration.

Jellymesh authenticates with `POST /Users/AuthenticateByName` and uses the
resulting access token. The password and the token are registered with the
audit redactor, and the node authenticates again when the token is refused.
It reads:

- libraries from `GET /Users/{user}/Views`;
- items from `GET /Users/{user}/Items`, a published library at a time and a
  page at a time, with provider IDs, paths, and ETags;
- an item's library, at request time, from `GET /Items/{item}/Ancestors`.

Phase 0 showed that Jellyfin's library permissions gate metadata but not the
raw stream route. So the service user's restrictions cannot be the only check
on a per-item request. Every request that names an item (artwork now,
streams in Phase 3) re-resolves that item's library from live Jellyfin
state, and is refused unless that library is currently published. That also
covers an item moved between libraries (plan-review B4).

A **protected set** of library IDs (`JELLYMESH_PROTECTED_LIBRARIES`), with
Cedar's `Family Movies` as its first entry, can never be published even if
the service user can see it. This is the general default-deny rule of
plan-review C2. A library is exposed only by explicit publication, and the
protected set is a second barrier for the libraries that matter most.

#### Publication (A-9)

The operator publishes a library by its stable Jellyfin ID and declares its
**root paths**:

```text
jellymesh publish <library-id> -root /media/movies [-root ...]
```

Publication is refused in three cases: the service user cannot see the
library, the library is protected, or any current item lies outside the
declared roots. Afterwards, every refresh checks every item against the
roots. An item outside them is withheld, and the whole publication **pauses**
until the operator publishes again with corrected roots. This closes the
accident plan-review C1 describes: someone adds a path to a published
library, and everything under it federates silently.

#### The source catalog

The source keeps a catalog of what it publishes, refreshed from Jellyfin
every catalog interval (hourly by default) and on demand.

- Every item has a **revision**. It increases when the item's ETag or
  normalized metadata changes.
- Every change takes the next value of a node-wide **change sequence**.
- An item that no longer appears in a published library becomes a
  **tombstone** with its own sequence. So does every item of a library that is
  unpublished or paused.
- Within a refresh, parents take sequences before their children (series,
  then seasons, then episodes), so a consumer that applies changes in order
  never sees a child before its parent.

#### The catalog API

Two member-only routes serve it, authorized per group like the log routes:

```text
GET /jellymesh/v1/groups/{group}/catalog/libraries
GET /jellymesh/v1/groups/{group}/catalog/changes?after=S&limit=N&exclude=L1,L2
```

- **libraries** lists what the source publishes and has not paused, with
  item counts, for the destination's opt-out interface.
- **changes** returns up to `limit` changes after sequence `S`, in order. The
  destination names the libraries of this source it has opted out of in
  `exclude`, and the source never sends their metadata.
- A destination the source has blocked gets 404. Blocks are pairwise and
  symmetric.

The responses are authenticated by the mutual-TLS session, which is pinned to
the source's key. That satisfies the requirement for authenticated manifests
without a second signature: the session already binds every response to the
source.

#### The destination catalog

A destination keeps, per source, a cursor (the last change sequence applied)
and a remote-catalog record for every item.

- Changes are upserted idempotently. A change whose revision is older than
  the stored one is rejected.
- A failed or partial sync keeps the cursor and every known-good record.
- A tombstone removes the item at once, and writes a retention record that
  keeps its identity for the grace period.
- The destination drops any item from a library it has opted out of, or from
  a protected-looking library it did not expect, even if a misbehaving source
  sends it.
- Opting out removes that library's records locally. Opting back in refetches
  that library from sequence zero.

### Music identity

Music requires an early feasibility gate. The initial rules are:

- prefer MusicBrainz and other strong provider IDs;
- preserve artist, album, disc, track, and source-version relationships;
- do not merge albums or tracks on title alone;
- determine whether Jellyfin can expose alternate source files under one logical album/track presentation;
- if stock clients cannot represent the required grouping, record the exception rather than creating misleading duplicate cards.

## 10. Generated media and version grouping

The consumer Jellymesh Service creates a generated root for each collection type and adds those roots to the destination’s existing corresponding Jellyfin libraries. The destination library, its users, and its collection plugins remain the owners of local collections. Jellymesh publishes media and metadata only; it does not publish or synchronize source collections or playlists.

A single ordinary Jellyfin library cannot provide normal Movies, TV Shows, and Music top-level views at the same time. The generated layout remains partitioned by collection type so each generated root can be added to the correct existing library:

```text
Friends/
  Movies/
    Movie (2020) [tmdbid-12345]/
      movie.nfo
      Movie (2020) [tmdbid-12345] - Cedar.strm
      Movie (2020) [tmdbid-12345] - Walnut.strm
  TV Shows/
    Series (2018) [tvdbid-12345]/
      tvshow.nfo
      Season 01/
        series.s01e01.nfo
        Series S01E01 - Cedar.strm
        Series S01E01 - Walnut.strm
  Music/
    Artist/
      artist.nfo
      Album/
        album.nfo
        tracks and source references
```

Collections are destination-local. A source may expose item provider IDs such as TMDB, TVDB, IMDb, or MusicBrainz, but Jellymesh must not copy source collection membership or create/update collections on the source server. The destination’s normal Jellyfin collection mechanisms and local plugins determine which items belong in local collections.

Music playlists are also destination-local. A local user may create a playlist containing any locally visible songs, including remote songs materialized into the destination’s Music library. Jellymesh does not replicate those playlists to peers.

The implementation must not assume that filename and NFO behavior alone guarantees grouping on every Jellyfin client. Version grouping, collection behavior, music behavior, and source labels are Phase 0 exit criteria.

The server name is represented first in the native version label. Standard metadata fields can be used as fallbacks after client testing. As built, a `From <source>` tag is that fallback, and the only label an episode has (section 9, C-MA-7). A circular avatar is optional and must not compromise the stock-client guarantee.

Deduplication is identity-first. Jellymesh may merge source media into one logical work only when strong identity matches, such as the same media type plus TMDB, TVDB, IMDb, or MusicBrainz identifier. Title/year matches are insufficient and remain separate. Jellymesh must not automatically prefer one source’s version of a film; native Jellyfin version selection remains the user-facing control. Episodes are the exception: Jellyfin shows no episode versions, so each episode plays from one chosen source (conformance A-13).

### Generated content and history preservation

Generated remote content consists only of Jellymesh-owned federation material:

- `.strm` references to source media;
- generated NFO metadata;
- Jellymesh-managed remote artwork and metadata caches;
- source-to-destination catalog indexes and mappings;
- sync cursors and source-health state.

Jellymesh does not copy the actual media files. A confirmed source tombstone immediately removes the generated item from the visible Jellyfin library. The receiving Jellymesh Service retains a separate retention record containing metadata, artwork, logical identity, and history through the configurable grace period so the item can be restored if it returns. The local Jellyfin database, local media, local users, local collections, local playlists, and local user data are not disposable generated content.

Before removing generated remote artifacts, the receiving Jellymesh Service must preserve per-user playback history in a local history ledger keyed by logical work identity rather than by a source server or generated Jellyfin item ID. The ledger should retain at least:

```text
local_user_id
logical_work_id
media_type
provider_identity
played
play_count
playback_position_ticks
last_played_at
```

The logical work identity should use strong metadata such as TMDB, TVDB, IMDb, or MusicBrainz where available. When the same work later returns from another server, a different Jellymesh library, or a local copy, the ledger can reapply the local user’s watched state, play count, and resume position. Existing local state is authoritative: Jellymesh fills only missing fields and never overwrites newer local progress. The broader history model may later include favorites and ratings, but those are not required for the first restoration path. If no reliable logical identity exists, Jellymesh must not guess based only on title and year.

Group dissolution or source ejection may purge generated references immediately while retaining the history ledger. The retention record for a source deletion is purged only after the configured grace period if the item does not return. Local Jellyfin user data remains the source of truth while the ledger provides restoration across generated-item removal and reappearance.


#### History as implemented (Phase 4, A-14)

Status: assumed (conformance.md A-14 and A-15), 2026-09-28, on a probe of
Jellyfin 10.11.11 recorded as conformance M-10
(`lab/identitycheck/run-throwaway.sh`).

The ledger described above is not built. It would need to read and write
every local user's state, which a non-administrator service user cannot do
(A-8), and Jellyfin already does the job: when an item is removed it detaches
its users' state, and reattaches it to an item that appears with a shared
identifier. Watched state, play count, resume position, and favourites came
back for films returning under another folder and source, and for episodes,
across a restart. Jellyfin's state is also per local user, which is what
C-HI-1 asks.

What Jellymesh must do is keep what Jellyfin keys on stable:

- one work is one item, since two items with one key share a single retained
  row (M-10);
- a work keeps its folder, since a new path is a new item;
- a show keeps the identifiers it was first written with, since Jellyfin keys
  episode state under the series' TVDB identifier before its TMDB one and a
  show that gained TVDB lost its episodes' state in the lab.

Work pins (store table `work_pins`) hold both for as long as a work is
materialized and for 90 days after, matching the age after which Jellyfin's
clean-up task may delete detached state. That task has no default trigger.
Local and remote copies of one film stay separate items with separate state
(A-15), because the merge API refuses a non-administrator.

## 11. Playback

The initial playback design is relay-backed:

1. A local client starts playback against the local Jellyfin server.
2. Jellyfin resolves the generated `.strm` reference.
3. The local Jellymesh Service validates the local reference and requests the source from the owning Jellymesh Service.
4. The source Jellymesh Service authenticates the destination peer, validates the current library publication and that the destination has not opted out, and retrieves the source media through the local Jellyfin deployment.
5. The source Jellymesh Service returns the media stream to the consumer Jellymesh Service.
6. The consumer Jellymesh Service returns the stream to the local Jellyfin server, which serves or transcodes it to the client.
7. Playback progress is written only by the local Jellyfin server.

The relay must support:

- HTTP HEAD;
- byte ranges;
- cancellation;
- backpressure;
- content type and length where known;
- bounded retries;
- no full-file buffering;
- no arbitrary remote URL proxying;
- clear source-unavailable errors;
- protection against token leakage in logs and generated files.

Direct client-to-provider playback may be evaluated later. It would reduce consumer egress but would require a different authorization and client-compatibility model. Direct play is unrestricted in v1; the default remote-transcoding policy allows one concurrent transcode per destination, with configurable overrides.

### Materialization and relay (Phase 3)

Status: assumed (conformance.md assumptions A-10 to A-12), 2026-09-28. It
rests on a probe of Jellyfin 10.11.11 recorded as conformance M-8
(`lab/materializecheck/run-throwaway.sh`).

#### The generated layout (A-11)

The destination writes one generated root per collection type, and the
operator adds each root to the matching existing Jellyfin library once:

```text
<generated>/Movies/<Title> (<Year>) [jmid-<id>]/
    movie.nfo
    <Title> (<Year>) [jmid-<id>] - Cedar.strm
    <Title> (<Year>) [jmid-<id>] - Cedar.en.srt
    <Title> (<Year>) [jmid-<id>] - Walnut.strm
    poster.jpg
<generated>/TV Shows/<Series> (<Year>) [jmid-<id>]/
    tvshow.nfo
    poster.jpg
    Season 01/<Series> S01E01.strm
    Season 01/<Series> S01E01.nfo
```

- **Folder IDs.** `<id>` is a Jellymesh-internal identifier derived from the
  work's identity. No provider identifier appears in a path (A-2): Phase 0
  showed a `[tmdbid-…]` folder tag pulling unrelated, even adult, titles into a
  library. Provider identifiers go in the NFO only, as both `<uniqueid>` and
  the legacy `<tmdbid>`, `<imdbid>`, `<tvdbid>` elements, because on 10.11.11
  only the legacy elements took effect.
- **Works.** Items are one work when they share a TMDB, TVDB, or IMDb
  identifier and disagree on none, films and series separately (Phase 4,
  C-HI-6). A source that knows a film only by IMDb joins one that knows it by
  TMDB and IMDb; two items with one IMDb identifier and different TMDB ones
  stay apart. The NFO carries every identifier the work's items know. An item
  without a strong identifier is a work of its own, and never merges on title.
- **Versions.** A film shares one folder across sources. Each file is named
  `<exact folder name> - <source name>`, so Jellyfin shows one item with one
  version per source, labelled with the source's name (M-8). A second copy
  from one source is labelled `<source name> 2`.
- **Episodes.** A series is one show folder across sources, holding every
  source's episodes. An episode is addressed by its series work, season, and
  episode number, and has one file from one source, because Jellyfin shows no
  episode versions (M-10, A-13). The source already playing an episode keeps
  it while it remains; otherwise the first source by node ID is chosen. The
  file name carries no source, so a change of source keeps the path. An
  episode without numbers is never grouped.
- **Source labels and metadata.** Every NFO names the item's sources as
  `From <source>` tags, which Jellyfin shows on the item page and offers as a
  library filter: a film every source it has a version from, a show the
  sources its episodes play from, and an episode the one it plays from, since
  an episode has no version label. The NFO also carries the source's
  studios, first tagline, and community and critic ratings (C-MA-7, M-8).
- **Pins.** A work keeps the folder it was first materialized under, and a
  show keeps the identifiers its NFO was first written with, while it is
  materialized and for 90 days after, because Jellyfin keys a user's state on
  the path and the identifiers (see "History" below, A-14).
- **What a file holds.** A `.strm` holds only a local relay URL with an opaque
  reference. It never holds a peer address, a key, or a token (C-PR-3).
- **Subtitles and artwork.** External subtitles are copied beside their
  `.strm`, because Jellyfin must find a subtitle file on disk to list it
  (Phase 0, C-MA-3). Embedded subtitles travel inside the stream. The source's
  primary image is copied as `poster.jpg`.
- **Atomic writes.** Every file is written to a dot-prefixed temporary name
  and renamed into place. M-8 confirmed Jellyfin ignores dot-prefixed files.
  The NFO is written before the `.strm`, so Jellyfin never finds a reference
  without its metadata (C-MA-1).
- **Removal.** Removing an item deletes only paths inside the generated root,
  and revokes the relay reference in the same step.

#### Detection by Jellyfin (A-12)

Jellymesh cannot make Jellyfin rescan. A non-administrator is refused
`/Library/Refresh` and `/Items/{library}/Refresh` (403). In the lab the
real-time monitor did not notice new files within two and a half minutes,
even for a real media file on a real disk. `POST /Library/Media/Updated` is
accepted from a non-administrator (204) but had no effect.

So Jellymesh sends that notice anyway, since it is cheap and may help where
monitoring works, and relies on Jellyfin's scheduled library scan, whose
interval the operator sets (hourly is suggested). What this means in
practice:

- a newly materialized item appears at the next scan;
- a withdrawn item stops being **playable at once**, because its `.strm` and
  its relay reference are gone;
- a withdrawn item stops being **listed** at the next scan.

#### The relay (A-10)

The destination runs a local relay. Jellyfin fetches `.strm` URLs from it
with plain HTTP, and it forwards each request to the source over mutual TLS.

- **Authorization.** A local user can read a `.strm`'s URL through Jellyfin
  (Phase 0), so the URL must not be a credential, and a loopback bind alone is
  not enough once Jellyfin and Jellymesh run in separate network namespaces.
  The relay therefore accepts a request only from an allow-listed client
  address (loopback by default; the deployment adds Jellyfin's address), and
  only for a 128-bit random reference issued by the materializer and not yet
  revoked. A reference names an item without authorizing anyone. The
  allow-list and the destination's own policy decide.
- **Checks on every request.** The destination re-checks its policy: the
  source is a member, is not blocked, and the library is published and not
  opted out. The source re-checks the caller's membership and block, and
  re-resolves the item's library from live Jellyfin state
  (`sourcecatalog.Authorize`), because Jellyfin's stream route ignores
  library permissions.
- **The media route.** The source serves
  `GET|HEAD /jellymesh/v1/groups/{group}/media/{item}` by relaying Jellyfin's
  `/Videos/{item}/stream?static=true` as the service user. M-8 confirmed this
  route honours ranges and HEAD for a non-administrator. Subtitles and images
  have sibling routes.
- **Stream behaviour.** The relay passes through `Range`, `HEAD`,
  `Content-Range`, `Content-Length`, `Content-Type`, and `Accept-Ranges`. It
  never buffers a whole file, and it stops reading from the source as soon as
  Jellyfin disconnects (C-PB-1). An unreachable source answers 502 with a
  plain reason and changes no catalog or artifact state (C-PB-2).
- **Head cache.** Jellyfin probes a remote item on every PlaybackInfo, which
  clients send when an item's page opens, and each probe reads about a
  megabyte from the start (Phase 0, A1-a). So the relay keeps the first 4 MiB
  of each item it has served, per catalog revision, on disk under a total cap
  with least-recently-used eviction (`JELLYMESH_HEAD_CACHE_MB`, 1 GiB by
  default). A request starting inside the head is answered from it.
  Anything past the head is fetched with one range request starting where the
  head ends, opened only if the client is still there a moment after the head
  was sent. The cache saves bytes and never decides access. Every hit is first
  confirmed with the source by a bodiless HEAD, which runs the source's full
  authorization and shows the file is still the size the head was cut from.
  So a probe costs a HEAD instead of a megabyte, and a block, a withdrawal,
  or a replaced file takes effect immediately (C-PB-5). A withdrawn item's
  head is deleted with it.
- **Bandwidth ceiling.** The source enforces a per-destination ceiling on the
  bytes it sends (C-PB-3). Serving a stream above the ceiling as a source-side
  transcode, the second half of A-1, is a later step (C-PB-4).

### Presentation through a virtual filesystem (decided, A-17)

Status: **decided** with the user 2026-09-30, after the spike (#60) passed
all seven gates, including a 791-cycle, 13.5-hour soak. The spike's code
and measurements are in `lab/fusespike/`. Built into Jellymesh under the
"FUSE presentation" issues (#63): the mount, the read service, and the
materializer's descriptors are in (C-FS-1 to C-FS-8); the self-test,
pacing, deployment, and the rerun of the gates are in progress. Until a
node is switched, it uses `.strm`, which stays as the fallback for hosts
that fail the self-test.

**Why.** Some clients play a `.strm` item's URL themselves instead of asking
Jellyfin for it. The Jellyfin Roku app does this for every remote source
(`LoadVideoContentTask.bs`, M-1), so the TV fetches Jellymesh's loopback
relay from itself and fails. Presenting remote films to Jellyfin as ordinary
files makes them indistinguishable from local media. Every client then
streams through Jellyfin, and the spike showed the same Roku playing,
seeking, resuming, and switching subtitles (G4). Jellyfin also probes each
film when it is added, so the real size, runtime, codecs, and HDR details
are known before playback.

**Shape.** `JELLYMESH_PRESENTATION=fuse` selects it.

- **Descriptors in place of `.strm`.** The materializer writes the same
  generated root as for `.strm`, with the same layout, grouping, identity,
  pins, NFOs, posters, and subtitles. Each film's `.strm` becomes a small
  descriptor, `<film>.<ext>.jmfilm` (package `filmfile`), holding the
  item's reference, the film's size and average bitrate, and the
  modification time to show. Nothing in it is an address or a credential.
  Sources now send each item's file size, bitrate, and extension in the
  catalog, never its path; for a source that does not, the destination asks
  with a bodiless request. A film whose size cannot be learned is not shown.
  The time stays put while the film is unchanged, because Jellyfin probes a
  film again whenever it moves (C-FS-1).
- **A small mount process**, `jellymesh mount`, in its own container: the
  only piece holding `/dev/fuse` and `CAP_SYS_ADMIN`, and holding no key,
  token, or address. It shows the generated root read-only, each descriptor
  as its film, and hides descriptors and the materializer's temporary
  files. Listings, sizes, and small files come from local disk, so a library
  scan never waits on a source, and a change on disk shows within the
  kernel's cache time (a minute) without a remount (C-FS-2). Inode numbers
  are the mount's own, per path, because a disk reuses a deleted file's
  number.
- **Film bytes** come from the daemon's read service (package `filmread`)
  over a local socket that only the daemon's user and root can open. It
  fetches from the source through the existing media route, so the source
  still authorizes every fetch and the per-destination ceiling and direct
  paths apply. It fetches 1 MiB chunks into a bounded cache (256 MiB by
  default, `JELLYMESH_READ_CACHE_MB`), and reads ahead only once reading is
  sequential (doubling up to 16 chunks), so a probe costs about 1 MB and
  playback is not paced by round trips (C-FS-3). The relay's head cache is
  not used: Jellyfin probes a file only at scan time, not on every
  PlaybackInfo as it does a `.strm`.
- **Authorization.** Every read resolves the reference and asks the
  destination's policy, cached chunk or not, so a revocation, a block, or
  an opt-out stops reads at once. A chunk is served for at most a minute
  after it was fetched, which bounds how long a refusal at the source can
  go unseen (C-FS-4).
- **Mount propagation.** The mount reaches Jellyfin's container through a
  bind with slave propagation, so a remount appears there without
  restarting Jellyfin.

**Robustness.** Each point was proven by the spike's gates:

- Every read has a hard deadline (10 s) and fails with an I/O error, never
  a hang (G1, C-FS-3).
- The mount asks the kernel for a request timeout (`FUSE_REQUEST_TIMEOUT`,
  20 s). If the process itself deadlocks or freezes, the kernel aborts the
  connection instead of leaving Jellyfin threads in uninterruptible sleep
  (G1, C-FS-7). go-fuse 2.11 does not send the timeout, so Jellymesh carries
  a patched copy in `third_party/go-fuse` (#65) until the change is
  upstream. The kernel feature is recent (Linux 6.14): the mount refuses to
  run on a kernel without it. The kernel checks for expired requests every
  15 s, so a frozen mount fails its callers within the timeout plus 15 s.
- A watchdog reads a direct-I/O health file through the mount every 5 s. If
  the mount stops answering, the process exits; the container's restart
  policy starts it again, and startup detaches the dead mount (G1, G5).
- An absent or dead mount reads as an inaccessible library root, and
  Jellyfin keeps the items. In the spike no scenario lost an item or a
  watched state (G2).
- If a film's read fails, the read service records it, as does the mount
  when its deadline passes, and retries it every 10 s. Once it is readable
  again the materializer moves its descriptor's time forward an hour, so the
  next scan probes it; Jellyfin ignores a one-second change (G2-E). The
  record survives a restart (C-FS-5).
- Film contents are served only to Jellyfin's uid. Every other process sees
  names and sizes but cannot read, so backups, indexers, and file shares
  cannot pull films (G3). Jellyfin must therefore run as a dedicated uid.

**Hard requirement: extraction off.** With trickplay or chapter-image
extraction enabled, the spike's trickplay task pulled 104 MB a minute per
film, and would have pulled every film whole (G3). Remote films must live in
their own libraries with extraction off. A non-administrator service user
cannot read library options (A-8), so Jellymesh cannot verify this itself.
The read service is a second line of defence (#69, C-FS-10):

- Each film is read at full speed for a burst: ten minutes of play at its
  average bitrate, at least 64 MiB.
- Beyond the burst, reads are paced to four times the film's average
  bitrate, which the catalog carries, and never slower than 1 MiB/s, so
  that no single read waits near the mount's 10 s deadline.

Playback and transcoding need no more than real time, so they never wait:
a viewer reading at the film's own pace refills the burst faster than it
drains. A whole-film extraction costs about four viewers' worth of
bandwidth instead of the source's full uplink. That playback and
transcoding through Jellyfin are unaffected is to be shown by rerunning G3
against the build (#73). `jellymesh status` shows the total time reads
have waited (`reads.paced_seconds`).

**Choosing the mode (A-18).** The operator sets `JELLYMESH_PRESENTATION`,
`strm` by default, from the self-test, `jellymesh mount -check`, run in the
mount container at setup and after each update. It reports by name whether
`/dev/fuse` opens, a filesystem mounts with the container's privileges, the
kernel offers request timeouts, and the mountpoint's propagation is shared.
Jellymesh never switches the mode itself, and never per incident: a switch
changes every item's path, which Jellyfin sees only at its next scan.
`jellymesh status` shows the mode and the mount's reports, and warns when
the mount is silent or its propagation would hide remounts. Whether the
mount is visible inside Jellyfin's container is checked by the operator
(`docs/operator-fuse.md`), since the service user cannot browse Jellyfin's
filesystem. Moving an existing node from `.strm` to FUSE recreates every
generated item at a new path, and history should reattach by provider
identifier (A-14); M-14 tests it.

**Costs.**

- About 1 MB per film when it is added, taken from sources' uplinks. With
  twenty peers and 40,000 items that is about 40 GB, once.
- Linux hosts only. It will not run on Docker Desktop for macOS or Windows,
  on Windows-native Jellyfin, or on NAS systems whose Docker lacks FUSE.
- Two containers instead of one, a privileged mount container, and a patched
  dependency until the patch is upstream.

**Measured in the spike** (Jellyfin 10.11.11, kernel 7.2, 40 ms of added
latency):

| | FUSE | `.strm` |
|---|---|---|
| Start (first 1 MB) | 0.13 s | 0.05 s |
| Seek to the middle | 0.10 s | 0.06 s |
| Sustained | 322 MB/s | 783 MB/s |

Streams failed within 2 to 35 s under every failure injected, with
Jellyfin's API responsive throughout.

## 12. Availability and heartbeat behavior

Each Jellymesh Service maintains provider state:

```text
available
degraded
unavailable
unknown
```

State should include:

- last successful health check;
- last successful catalog sync;
- last failure time;
- failure category;
- consecutive failures;
- current revocation state.

Use heartbeat hysteresis rather than marking a provider offline after one failed request. Retain known-good metadata during transient failure. A source outage should prevent new playback from that source but should not cause destructive catalog pruning. Retain an offline source catalog for 15 days by default, matching the owner absence timeout.

A heartbeat and availability API are backend features. Stock Jellyfin clients do not have a standard provider-health card state. The initial stock-client behavior is therefore:

- retain the item for catalog continuity;
- fail playback cleanly when the source is unavailable;
- optionally suppress an unavailable media source if client testing shows that this is beneficial;
- expose detailed health in the Jellymesh management surface.

Greyed-out cards and circular source avatars require custom client work or a future Jellyfin-wide availability convention. They are stretch goals, not release gates.

## 13. Reliability model

The service should be designed for unattended home operation:

- persistent Jellymesh Service state and generated roots;
- atomic writes and safe shutdown;
- bounded retries with exponential backoff and jitter;
- health heartbeats every 5 minutes by default;
- incremental catalog synchronization every hour by default;
- per-peer sync jitter to avoid synchronized bursts;
- full catalog resynchronization only after cursor invalidation, source generation change, or explicit administrator action;
- per-peer rate limits;
- sync checkpoints;
- tombstones and deletion grace periods;
- disk-full and permission alerts;
- health and readiness endpoints;
- audit logs with redacted secrets;
- encrypted backup and restore of keys, configuration, publications, opt-out decisions, and sync state;
- graceful Jellyfin restarts;
- compatibility checks before upgrades;
- rollback instructions for Jellymesh Service and Jellyfin changes;
- no single central service required for normal operation.

The design should avoid claiming cloud-style availability for a single home server. The appropriate target is recoverable, observable, and independently operating nodes with eventual synchronization and safe degradation.

## 14. Conceptual Jellymesh Service API

The exact wire format is not finalized. The first protocol should be versioned HTTPS with narrowly scoped operations resembling:

```text
/jellymesh/v1/health
/jellymesh/v1/identity
/jellymesh/v1/catalog/manifest
/jellymesh/v1/catalog/items
/jellymesh/v1/catalog/series/{id}
/jellymesh/v1/images/{itemId}
/jellymesh/v1/media/{streamId}
/jellymesh/v1/revocations
```

The API must not accept arbitrary URLs from peer manifests. All source locations, stream references, image references, and catalog roots are resolved through server-side policy and stable source IDs.

## 15. Deployment direction

The eventual supported deployment should be appliance-like:

- Docker Compose or an equivalent container package;
- persistent data and generated-media volumes;
- a non-root service identity;
- automatic health checks;
- automatic restart;
- explicit backup and restore;
- guided enrollment, publication, and opt-out management;
- clear warnings before public exposure;
- no requirement for users to edit JSON or reverse-proxy rules for the normal path.

Each node needs a public HTTPS hostname and certificate. The public Jellymesh Service endpoint should expose only the required federation and media routes. Jellyfin administration should remain private or protected by the home’s normal administrative boundary.

## 16. Phased delivery

The first implementation target is a two-node vertical slice. It must cover invitation and approval, one published source library, catalog materialization, playback through the relay, and health synchronization. Music polish, richer metadata, and advanced history follow after that slice is proven.

### Phase 0: feasibility and lab

- Establish two disposable local Jellyfin nodes on Walnut.
- Use synthetic data and no real family media.
- Validate native movie and TV version grouping.
- Validate music behavior.
- Validate source-name rendering in the client matrix.
- Validate public HTTPS, Range requests, seeking, subtitles, and local progress.
- Validate deny behavior for a protected `Family Movies` library.
- Validate pairwise block behavior and destination-side library opt-out.
- Produce a go/no-go decision for the `.strm`/NFO architecture.

### Phase 1: node foundation

- Implement node identity and key storage.
- Implement pairwise enrollment, group membership, leave, and block state.
- Implement container deployment and health checks.
- Implement configuration backup and restore.
- Implement audit events and redacted logging.

### Phase 2: source adapters and publication policy

- Query source Jellyfin libraries and metadata.
- Add exact-library publication into the group pool.
- Add destination opt-out decisions per source library.
- Implement protected-library enforcement.
- Implement signed/versioned manifests.
- Implement incremental, paginated catalog sync.

### Phase 3: receiver materialization and relay

- Generate Jellyfin-readable artifacts atomically.
- Create the generated roots and integrate each collection type into the destination’s existing Jellyfin library.
- Implement authenticated range-capable media relay.
- Implement safe retries, cancellation, and source-unavailable behavior.
- Prove Family Movies is never materialized or playable.

### Phase 4: identity, grouping, and progress

- Implement strong work identity and alternate versions.
- Implement server labels and source metadata.
- Keep progress, favorites, ratings, and history local.
- Validate client-specific version selection and music behavior.

### Phase 5: hardening and pilot

- Run public HTTPS and failure-injection tests.
- Run backup, restore, key rotation, and rollback drills.
- Run a multi-home pilot with reboots, outages, certificate renewal, and library changes.
- Document supported versions and unsupported client behavior.

### Phase 6: stretch UX

- Add management-surface heartbeat and offline detail.
- Explore provider-health rendering in Jellyfin Web.
- Explore a client-independent source avatar convention.
- Explore direct client-to-provider playback if it provides meaningful value.

## 17. Release gates

A release is federation-ready only when:

- every source library is private by default;
- published-library pool entries and destination opt-out decisions work;
- owner/admin ejection removes future group participation without affecting unrelated peers;
- block actions stop media sharing for that pair without removing either node from the group;
- opted-out libraries are absent from remote catalogs, search, images, subtitles, and playback;
- Family Movies is absent from remote catalogs, search, images, subtitles, and playback;
- unauthorized peers cannot enumerate or stream content;
- user authentication headers and tokens are not forwarded between homes;
- public HTTPS works without Tailscale;
- stock supported clients can browse and play;
- movie and TV multiple-version grouping works;
- music behavior is proven or explicitly accepted as an exception;
- source names are visible in the agreed native UI locations;
- local progress remains isolated per local user;
- generated content can be purged without losing per-user watched state, play count, or resume position;
- transient outages do not destroy known-good state;
- revocation, backup, restore, and key rotation have been tested;
- logs and generated artifacts contain no unnecessary secrets.

## 18. Follow-up decisions

The unresolved product and protocol decisions are tracked in [follow-up-decisions.md](follow-up-decisions.md). The feasibility discoveries and rejected shortcuts are tracked in [discovery-log.md](discovery-log.md).
