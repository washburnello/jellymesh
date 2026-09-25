# Jellymesh Follow-up Decisions

This document records decisions that are settled enough to continue planning and questions that should be resolved before implementation is treated as final.

## Confirmed

- Target experience: stock Jellyfin clients and UI.
- Client matrix: Jellyfin Web, Android, Android TV, iOS/Swiftfin, and Roku TV are the day-one compatibility targets; other clients are best-effort until tested.
- Admin settings: the Jellymesh settings page includes published libraries, per-source opt-outs, remote-transcode limits, invitation/role management, and sync health; group-wide defaults are editable only by owners/admins.
- Group settings: group defaults apply automatically to reachable nodes without an explicit local override; local overrides win and changes are auditable.
- Topology: each server is an independent hub in a full mesh.
- Transport: public HTTPS is acceptable; Tailscale is not required.
- Peer authentication: mutual TLS, with the node's long-term Ed25519 key as the client certificate key. Authorization matches the certificate's key fingerprint against the pairwise trust record, never the hostname. Each node terminates its own TLS; a fronting component must use TCP/SNI passthrough rather than TLS termination. Chosen over signed request envelopes and pairwise bearer tokens because it adds no clock dependence, keeps node identity and transport identity as one object, covers streaming without per-request work, and introduces no new bearer secret.
- Key rotation and compromise recovery: there is no rotation protocol. A rotation event signed by the outgoing key fails precisely in the compromise case, because a stolen key can sign its own replacement. Recovery is by re-enrollment: eject the member, generate a fresh identity, and readmit through a new invitation and fresh approval. The new fingerprint makes it a different peer by construction. The cost is loss of continuity and a full catalog resynchronization, accepted for a group of roughly twenty people who can contact each other out of band. A printed offline recovery key is the natural later addition, not a first-release requirement.
- Content: movies, TV, and music are in scope.
- Privacy: Cedar’s `Family Movies` must never be exposed remotely.
- Publication: source libraries are not contributed by default; the source owner explicitly publishes selected libraries into the group pool.
- Grouping: one logical card with multiple source versions is preferred.
- Deduplication: merge only on strong provider identity; keep ambiguous items separate; do not automatically prefer one source’s version.
- Source identity: the owning server’s friendly name should be shown.
- Source display: v1 uses the friendly server name in native item details and version selection; quality/edition suffixes and a circular avatar are deferred.
- Enrollment UX: one-time pairing supports both a short code and a QR code.
- Invitation workflow: any active member can create an invite from the Jellymesh settings page; an owner or admin approves or denies the redeemed invite.
- Owner/admin availability: a redeemed invitation remains pending while all owners/admins are unavailable; it is not automatically denied.
- Invitation expiration: expiration applies until an invite is redeemed; a redeemed request remains pending until owner/admin action.
- Membership: the group is opt-in; any member can leave, but only an owner or admin may eject a member.
- Group roles: the creator becomes owner; the owner may promote members to admin. Admins can perform group operations but cannot eject other admins or remove the owner.
- Owner succession: if the owner is unavailable for 15 days, the oldest administrator (earliest promotion; member ID breaks exact timestamp ties) becomes owner. If no owner or admin remains, the group dissolves after 15 days with notifications every 5 days.
- Owner succession mechanism: succession is claimed, never applied on a local timer. The absence window only makes a claim eligible; installing a new owner additionally requires absence attestations from a strict majority of the members other than the absent owner, with the claimant's own observation counting as one voice (three members need one attestation, five need two, twenty need nine). Self-attestation, attestation by the absent owner, non-member attestation, and duplicates are rejected. This preserves the confirmed rule about *who* succeeds and fixes only *how* nodes agree the moment has arrived, which is what allowed split-brain ownership.
- Listener split: the Jellymesh Service runs three listeners. Federation is public and mTLS-authenticated; the relay is loopback-only and serves generated media to the co-located Jellyfin; the admin interface is loopback-only. Sharing an address between federation and either of the others is a configuration error and is refused at load.
- Fail-fast configuration: enabling federation without a public hostname or a Jellyfin API key is refused at load rather than failing at first use.
- Ejection: owner/admin ejection revokes future group participation; other members independently purge the rejected server’s generated content and do not rely on that server for cleanup. Ejection cannot erase media or metadata already downloaded. Rejoining requires a new invitation and fresh approval.
- Blocking: a member can block a specific peer without removing it from the group; the block is a symmetric pairwise media-sharing cut. Unblocking restores normal pool behavior while preserving explicit opt-outs.
- Administration: each node needs a local operator interface to inspect peers, source libraries, health, publications, blocks, and library decisions.
- Library policy: published libraries are auto-accepted by destinations; destination administrators can explicitly opt out of any source library by source server plus source library. Rating and local-user filtering remain local Jellyfin responsibilities.
- Product purpose: Jellymesh exists to make approved media libraries available across the group; it does not manage local users, permissions, or household policy.
- Onboarding: source users explicitly choose which libraries to publish during installation/setup; nothing is contributed by default.
- Membership scope: every group member owns a Jellyfin/Jellymesh server; receive-only servers are not supported. A joining server must publish at least one non-empty library.
- Sync behavior: new content added to an already published library is automatically synchronized to destinations that have not opted out.
- Sync cadence: health heartbeats default to every 5 minutes; catalog synchronization defaults to hourly incremental changes with jitter, backoff, and no destructive cleanup from failed or incomplete syncs.
- Sync configuration: intervals are configurable per node, with owner/admin-provided group defaults.
- MVP priority: build a two-node vertical slice first: invitation/approval, one published library, catalog materialization, playback, and health sync; defer music polish, richer metadata, and advanced history.
- Offline catalog retention: temporarily unavailable source catalogs remain for 15 days, matching the owner absence timeout.
- Library availability: joining the group does not remove a member’s ability to opt out of any particular remote library.
- Group pool: each server may publish exact libraries into a shared group pool; published libraries are available to members by default, and each destination can opt out of individual source libraries.
- Progress: watched state, resume points, favorites, ratings, and history remain local to the local user’s server.
- History preservation: generated remote references and caches may be purged, but per-user watched state, play count, and resume position are preserved in a logical-work history ledger and reapplied when matching content returns.
- History merge: preserve existing local state, fill only missing fields, and never overwrite newer local progress.
- Favorites and ratings: include in the broader history model as a later-phase/stretch goal; initial restoration covers watched state, play count, resume position, and last-played time.
- Playback: v1 uses the relay-backed path `Provider Jellymesh Service → consumer Jellyfin/Jellymesh Service → playback client`; direct provider-to-client playback is deferred.
- Resource limits: direct play is unrestricted; each destination defaults to one concurrent remote transcode, with configurable overrides.
- Group access: source libraries are published into the group pool; destinations auto-accept published libraries and can opt out individually.
- Local access: source publication, destination opt-out, and local Jellyfin library permissions are separate controls; normal Jellyfin permissions decide which local users see remote media.
- Library presentation: remote media is integrated into the destination’s existing library for each collection type, so local and remote items can participate in the same native collections and version relationships.
- Collections and playlists: collections and playlists are destination-local; Jellymesh does not replicate or broadcast source collections/playlists.
- Reliability: the product should tolerate unattended home-server operation and recover safely from outages.
- Offline awareness: heartbeat and stale-provider handling are desired; visible greyed-out stock-client cards are a stretch goal.

## Assumptions pending ratification

These were open product decisions blocking implementation. Each is resolved by a
documented assumption in [conformance.md](conformance.md) section 9 so that work
can proceed, and each can be overridden. They are listed here because they are
decisions, not findings.

- **Source-side bandwidth ceiling (A-1).** A source enforces a configurable
  per-destination bitrate ceiling and requests a transcoded rendition from its
  own Jellyfin when the ceiling would be exceeded. Resolves plan-review A4,
  which is otherwise a v1 blocker: the relay is a byte-exact pass-through, so
  without a ceiling one 4K remux saturates the source household's uplink.
- **No provider identifiers in generated paths (A-2).** Generated directories
  use a Jellymesh-internal identifier; provider IDs travel in NFO only. Two
  synthetic identifiers each resolved to unrelated real titles during Phase 0,
  one of them adult.
- **Music is out of scope for v1 (A-3).** Federated music duplicated the artist
  and album tree and NFO metadata did not repair it. If it returns, the shape is
  a separate `Friends Music` library so duplication is contained.
- **Integrated libraries with a Jellymesh-driven merge graph (A-4).** The
  extraction risk that argued against integration was measured and disproven,
  and the Jellyfin merge API was validated as a mitigation. Jellymesh must own
  and maintain the merge graph.

## Follow-up questions

### Playback

1. Initial relay path: accepted for v1. Direct provider-to-client playback is deferred.

2. Direct provider-to-client playback: resolved as optional post-MVP optimization. Keep the relay-backed path as the supported v1 mechanism; evaluate direct playback later only if client compatibility and authorization can be proven without weakening access control.

### Availability and users

3. Group pool scope: resolved. Each source server publishes exact libraries to the group pool; destinations auto-accept published libraries and can opt out individually.

4. Which local users on a consumer server may see the integrated remote content?

   Recommendation: reuse normal Jellyfin library permissions. Source publication and destination opt-out control server-to-server access; Jellyfin controls local user access.

5. Group pool publication: resolved. A source server publishes exact libraries into the group pool; destinations auto-accept them and can opt out.

### Library presentation

6. Library presentation: resolved. Remote content is integrated into the destination’s existing library for each collection type. The destination’s local Jellyfin collection plugins and users own collection creation and maintenance.

7. Source display: resolved. The friendly server name appears in native item details and version selection; a circular avatar is deferred.

8. Source label content: resolved. Use the server name only for v1; quality/edition suffixes are deferred.

### Enrollment and trust

9. Enrollment UX: resolved. Use both a one-time short code and a QR code for the same invitation.

10. Peer removal: resolved. Any node can leave or revoke its own participation; an owner or admin may eject a member through a signed group revocation. Blocking is separate from removal.

11. Group roles: resolved. The creator becomes owner; the owner may promote members to admin. Admins can perform group operations but cannot eject other admins or remove the owner.

12. Owner succession: resolved. If the owner is unavailable for 15 days, the oldest administrator (earliest promotion; member ID breaks exact timestamp ties) becomes owner.

13. Group dissolution: resolved. If no owner or admin remains, wait 15 days with notifications every 5 days before dissolving the group.

14. Peer health visibility: resolved. All peers see aggregate health and sync status; detailed failure history, affected item counts, retry state, and sanitized diagnostics are visible only to the local administrator.

15. Block semantics: resolved. A block is a symmetric pairwise media cut; both nodes remain group members, and unblocking preserves explicit opt-outs.

16. Content filtering: resolved. Jellymesh only manages whole-library opt-out. Rating and local-user filtering remain local Jellyfin responsibilities.

### Content behavior

17. Source deletion: resolved. A confirmed source deletion immediately removes the remote item from the visible Jellyfin library. Jellymesh retains separate metadata, artwork, identity, and history data for 7 days by default, then purges it if the item does not return.

18. Offline catalog retention: resolved. A temporarily offline source remains visible for 15 days, with playback denied until the source recovers.

19. Preferred source version: resolved. Strong provider IDs can merge logical works, ambiguous title/year matches remain separate, and Jellymesh does not automatically prefer one source’s version. Native Jellyfin version selection remains available.

20. Playlists and collections: resolved. Playlists and collections are destination-local. Jellymesh publishes media and provider metadata only; it does not replicate or broadcast source collections or playlists.

### Deployment and operations

21. Deployment model: resolved. Jellymesh runs as a single sidecar container beside Jellyfin, with generated media roots; it does not modify Jellyfin internals or databases.

22. Public endpoint: resolved. The public Jellymesh Service endpoint is separate from the user-facing Jellyfin endpoint; expose only required federation/media routes and keep Jellyfin administration private.

23. Installer: resolved. The first installer assumes Docker Compose, while the Jellymesh protocol and state model remain independent of the installer.

24. Remote-transcode and bandwidth policy: resolved. Direct play is unrestricted; each destination defaults to one concurrent remote transcode, with configurable overrides.

## Decisions to revisit after Phase 0

- Native Jellyfin movie/episode version grouping.
- Native Jellyfin music alternate-source behavior.
- Exact source-name rendering across the chosen clients.
- Whether the relay path is acceptable to all participants.
- Whether the integrated-by-type library model is more usable than a separate Friends library family for each collection type.
- Whether the public Jellymesh Service can be deployed without exposing any Jellyfin administration surface.
