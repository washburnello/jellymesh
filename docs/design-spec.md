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

A block is not a global membership mutation and must not affect unrelated peers. A node that leaves or blocks a peer may still remain visible in other nodes’ group directories according to each node’s local policy.

An owner or admin may eject an ordinary member by publishing a signed membership revocation. Ejection removes the member from future group participation, invitations, catalog publication, and new playback authorization. It cannot erase media, metadata, or other data already downloaded by the member. The owner cannot be ejected. A rejected member must receive a new invitation and fresh approval to rejoin; an old grant or membership record is never silently reinstated.

An ejected server is untrusted and is not responsible for cleanup. The revocation is signed, sequenced, and propagated to the other group members. Every receiving node independently denies future access, removes its own generated Jellymesh artifacts and caches, triggers a local Jellyfin rescan, and verifies that the rejected server’s content is absent. The rejected server cannot restore access by withholding a catalog response, tombstone, or cleanup operation.

For a group of approximately twenty nodes, the owner and administrators form a bounded control plane while media and playback remain pairwise. Each node maintains its own peer directory, pairwise trust records, library publications, opt-out decisions, blocks, and health state. Full mesh connectivity is acceptable, but it must be bounded by pagination, incremental sync, rate limits, and backoff.

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

The server name is represented first in the native version label. Standard metadata fields can be used as fallbacks after client testing. A circular avatar is optional and must not compromise the stock-client guarantee.

Deduplication is identity-first. Jellymesh may merge source media into one logical work only when strong identity matches, such as the same media type plus TMDB, TVDB, IMDb, or MusicBrainz identifier. Title/year matches are insufficient and remain separate. Jellymesh must not automatically prefer one source’s version; native Jellyfin version selection remains the user-facing control.

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
