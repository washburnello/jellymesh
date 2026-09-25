# Jellymesh Discovery Log

Date: 2026-09-23

## Environment

- Cedar is reachable through the configured `ssh cedar` alias.
- Cedar runs `jellyfin/jellyfin:latest` with image digest `sha256:aefb67e6a7ff1debdd154a78a7bbb780fd0c873d8639210a7f6a2016ad2b35db`.
- The installed server reports `Jellyfin.Server 10.11.11.0`.
- Cedar’s Jellyfin container uses host networking and is reachable over the local/Tailscale address used during inspection.
- Walnut has Docker 29.7.2 and Docker Compose 5.5.1.
- Walnut had no Jellyfin container before this planning work.
- No production configuration, account, media library, or database was changed.
- The isolated terminal seat used for clean verification does not include Go. The project-image plan reports the Arch `go` package as missing; building the Jellymesh Service skeleton for verification requires explicit approval of that one-time project image change.
- The approved one-time project-image build failed before completing because the build VM could not resolve `stable-mirror.omarchy.org` while downloading the Arch `go` package. A retry failed with the same mirror-resolution error. No host package was installed and no test was run.

## Prior art

### JellyFed

Repository: <https://github.com/MaxBly/JellyFed>

JellyFed is the closest implementation to the original product idea. It is a Jellyfin 10.11.x/.NET 9 plugin that:

- exposes a catalog endpoint;
- materializes remote movies and series as `.strm` and `.nfo` files;
- uses multiple `.strm` files to let native Jellyfin group alternate versions;
- keeps media on the source peer;
- uses per-peer access tokens;
- supports local Jellyfin playback and progress.

Its current limitations are important:

- per-peer category toggles are not exact per-library grants;
- current work is early and v1 end-to-end validation is incomplete;
- source heartbeat state is administrative and is not rendered as a stock-client unavailable state;
- source labels are present in generated naming/metadata but not guaranteed as a card avatar;
- the repository currently reports no declared license, so its source should not be copied or forked without permission or a license clarification;
- the consumer Jellyfin server generally performs the HLS/transcode work.

The architecture is a useful reference and possible Phase 0 comparison candidate, not a dependency decision.

### External item provider proposal

Jellyfin PR: <https://github.com/jellyfin/jellyfin/pull/16494>

The proposal adds concepts that closely match a first-class native implementation:

- `SourceType.External`;
- an external item provider;
- external item metadata;
- stream redirect providers;
- availability-aware synchronization;
- time-limited signed stream tokens.

It is still draft work and is not a stable extension point for an ordinary Jellyfin plugin. The design therefore keeps Jellymesh’s presentation layer replaceable if Jellyfin gains these capabilities later.

### Jellyswarrm

Repository: <https://github.com/LLukas22/Jellyswarrm>

Jellyswarrm is a reverse proxy that presents multiple Jellyfin servers as one service. It demonstrates API merging, virtual IDs, health-aware routing, and direct playback from the origin. It also introduces a separate authentication/user model and a bundled management UI, so it is a reference for aggregation rather than the selected Jellymesh architecture.

### Remote browsing and indexing projects

JellyHub and oversea-fin show that cross-server catalog discovery is useful, but they do not provide the transparent native playback and version behavior required here.

## Offline-state finding

A stock Jellyfin client has no general provider-health field. Server-side work can:

- keep the item visible;
- block or fail playback;
- suppress an unavailable media source;
- return an unavailable error;
- omit the item entirely.

Those actions do not produce a reliable greyed-out card across Jellyfin Web, Android, Android TV, iOS, and other clients. A custom Jellymesh availability field would be ignored by stock clients.

The initial Jellymesh behavior should therefore be backend-first: preserve known-good metadata, expose health to administrators, and fail new playback cleanly when a source is unavailable. A custom Web client for greyed-out cards is a later feasibility project.

## Jellyfin library and collection constraint

Jellyfin’s official documentation describes a library as a virtual collection of media that can contain several filesystem locations, but a library still has one collection type. Jellyfin therefore cannot natively expose Movies, TV Shows, and Music as three normal top-level library views inside one ordinary library.

The implementation must choose between:

- adding generated remote roots to the existing Movies, TV Shows, and Music libraries, which is the best candidate for native cross-server collections and version grouping; or
- creating separate `Friends Movies`, `Friends TV`, and `Friends Music` libraries, which provides separation but should not be assumed to support collections spanning local and remote libraries.

For a franchise split across servers, the integrated-by-type model can make all five movies available to Jellyfin’s normal collection mechanisms, including local and remote copies. The Friends-library family may show the remote subset as a collection, but cross-library collection behavior must be tested before promising it.

The destination’s local Jellyfin instance owns collection creation and maintenance. Jellymesh exports media metadata and strong provider IDs so local metadata and collection plugins can do that work; it does not replicate source collection membership and does not create collections on providers.

Deduplication is identity-first: strong provider identity can merge a logical work, title/year alone cannot, and Jellymesh does not automatically prefer one source’s version. Native Jellyfin version selection remains the user-facing control.

Playlists are local, ordered references to items. A playlist created on one consumer can potentially reference any item that consumer can access, including remote items, but the playlist itself is not automatically a provider-owned shared object. The product decision is local-only playlists: a destination user may create a playlist from any songs visible in the integrated Music library, but Jellymesh never broadcasts that playlist to peers.

## Architectural consequence

The initial design is a Jellymesh Service that materializes approved remote content into a generated Jellyfin library root. This keeps federation state and the provider authorization model outside Jellyfin’s database while allowing stock Jellyfin clients to perform normal browsing and playback.

The biggest feasibility risks are:

1. whether native movie and episode alternate-version grouping is reliable;
2. whether native music alternate-source behavior is acceptable;
3. whether the source server name is visible in enough stock-client locations;
4. whether a public HTTPS Jellymesh Service can be deployed without exposing Jellyfin administration;
5. whether a consumer-side relay meets the group’s bandwidth expectations;
6. whether static `.strm` references can be refreshed safely without disrupting playback.
7. whether native Jellyfin collections can span the local and remote content model required for cross-server franchises.

## Group, block, and library policy model

The product needs three independent controls:

- **Membership**: a node is part of the opt-in group.
- **Publication**: a source makes an exact library available to the group pool.
- **Destination library policy**: the receiving node auto-accepts published libraries and can opt out of each exact source library.

No member can kick another member. A member can leave or block a peer locally. The block should be a pairwise media-sharing cut while both nodes remain group members; unblocking should restore normal pool behavior while preserving explicit opt-outs.

For a group of roughly twenty people, the design should not require a central owner or central control plane. Each node maintains its own peer directory, trust records, library publications, opt-out decisions, blocks, and health state. Full mesh connectivity is possible, but catalog traffic grows quadratically and must use incremental sync, pagination, rate limits, and backoff.

The operator interface should list each peer, health, source library names/types/counts/sizes, published-library state, block state, and per-destination opt-out decisions. The Jellymesh settings page also includes published-library controls, the remote-transcode limit, invitation/role management, and sync health. Group members see aggregate health and sync status; detailed failure history and sanitized diagnostics are visible only to the local administrator. Group-wide defaults apply automatically to reachable nodes without an explicit local override; local overrides win and changes are auditable. Defaults are editable only by owners and administrators. It should be a Jellymesh Service administration surface rather than a modification to the normal Jellyfin user experience.

Jellymesh does not perform item-level rating, genre, tag, or per-user filtering. Local Jellyfin users, permissions, parental controls, and household policy remain local concerns. An opted-out library must not leak through catalog, search, artwork, guessed item IDs, subtitle requests, or direct stream paths.

Effective federated visibility is:

```text
source publication AND NOT destination opt-out AND local Jellyfin library access
```

The source onboarding flow must ask the owner which libraries to publish. No source library is published by default. Every group member must be a server owner and must publish at least one non-empty library before admission. Once published, a library is automatically available to group destinations; destinations can explicitly opt out. New items added to an already published library follow the same automatic path. People who only want to consume media should use a local user account on a friend’s Jellyfin server instead of joining as a viewer-only Jellymesh server.

## Group owner and administrators

Every group has one owner and an explicitly assigned pool of administrators. The creator becomes owner. The owner may promote members to administrator. Administrators can approve invitations, eject ordinary members, and perform group operations, but cannot eject other administrators or remove the owner.

If the owner is unavailable for 15 days, the oldest administrator by promotion time becomes owner; exact timestamp ties are broken by member ID. Administrators are not randomly or automatically appointed; they are assigned by the owner.

Any active member can create a one-time invitation from the Jellymesh settings page; an owner or administrator receives the redeemed request and must approve or deny it. A redeemed request remains pending while all owner/admin decision-makers are unavailable rather than being automatically denied. Invitation expiration applies before redemption; after redemption, the request remains pending until owner/admin action.

If the owner is unavailable for 15 days, the oldest administrator by promotion time becomes owner; exact timestamp ties are broken by member ID. Administrators are not randomly or automatically appointed; they are assigned by the owner. If no owner or administrator remains, the group waits 15 days, sends notifications every 5 days, and then dissolves.

Benefits:

- much easier onboarding for friend-of-a-friend scenarios;
- one place to authenticate and audit invitations;
- a group can define admission rules such as requiring an explicit non-empty publication;
- an owner or administrator can eject an ordinary member from the signed group roster;
- existing members can keep using cached membership and peer links while the owner is temporarily offline.

Risks:

- the owner is the highest-privilege group administrator and cannot be removed by admins;
- a compromised owner or admin must not be able to impersonate peers or read media;
- invitation theft, replay, and impersonation require binding invitations to the invitee’s generated key and fingerprint;
- ejecting a member cannot erase media or metadata already downloaded;
- forcing members to publish content conflicts with privacy and should be an explicit group policy, not a universal Jellymesh rule;
- owner succession must avoid split-brain ownership.

Recommended boundary: the owner and admins may invite, remove ordinary members, publish policy, and sign membership events; all content and playback authorization remain pairwise between servers. Existing peers should cache and verify signed membership state, fail closed on unknown or stale revocations according to policy, and continue operating without the owner for already-approved connections. A future multi-owner or co-admin model could avoid depending on one owner.

A rejected server is untrusted and cannot be relied upon for cleanup. An owner/admin-signed, sequenced revocation must be propagated to every remaining member; each member independently denies future access, removes its own generated artifacts and caches, triggers a local Jellyfin rescan, and verifies the content is gone. The rejected server cannot restore access by withholding catalog data, tombstones, or cleanup work. Re-entry requires a new invitation, fresh approval, and a new admission event.

## Generated content and history

Generated remote content is limited to Jellymesh-owned `.strm` references, NFO metadata, remote artwork/metadata caches, catalog indexes/mappings, and sync state. Jellymesh does not copy actual media files. A confirmed source tombstone immediately removes the item from the visible Jellyfin library, while a separate retention record keeps metadata, artwork, logical identity, and history for 7 days by default. Local Jellyfin databases, users, collections, playlists, and user data must not be treated as disposable generated content.

Before generated artifacts are purged after source rejection or group dissolution, the receiving service must preserve per-user watched state, play count, resume position, and last-played time in a local history ledger keyed by logical work identity. Strong provider IDs are required for safe restoration; title/year alone must not be used to guess that content is the same. Existing local state is authoritative during restoration: fill only missing fields and never overwrite newer local progress. Favorites and ratings are planned for a later history-preservation phase, not the initial watched/progress path.

## Sync cadence and scale

The default operating model is a lightweight health heartbeat every 5 minutes and an incremental catalog sync every hour. Heartbeats detect reachability without downloading catalogs. Catalog sync uses a signed change cursor and requests only changes since the last successful sync; full resynchronization happens only after cursor invalidation, source-generation change, or explicit administrator action.

For a 20-member group, each node has up to 19 peer relationships. Heartbeats produce roughly 4,560 small health requests per hour across the group, while catalog sync produces about 380 incremental requests per hour. The traffic is acceptable when responses are small, compressed, paginated, jittered, and rate-limited. Failed or incomplete syncs must never become deletion events. A temporarily offline source catalog remains for 15 days by default.

## Current recommendation

Run a disposable three-part Phase 0 lab before committing to implementation:

1. two synthetic Jellyfin instances on Walnut;
2. a synthetic published library and a synthetic protected Family Movies library;
3. an owner/admin succession and member-ejection test;
4. stock-client tests for grouping, playback, seeking, subtitles, source labels, progress, and revocation.

Cedar should remain untouched during the initial local test. A later opt-in Cedar integration should use a backup and a separate Jellyfin test instance or controlled library configuration.
