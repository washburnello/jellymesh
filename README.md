# Jellymesh

Jellymesh is a federation layer for independent Jellyfin servers, under development. Each home server can publish selected libraries to approved peer servers while continuing to use ordinary Jellyfin clients and local Jellyfin user accounts.

## Current status

- Phase 0 (feasibility) is complete; see [Phase 0 results](docs/phase-0-results.md).
- Phase 1 (node foundation) is implemented. A node runs as a single container, forms a group with other nodes, admits new ones by invitation, keeps the replicated group log in step, derives transport trust from it, audits what it does, and backs itself up.
- Phase 2 (source adapters and publication policy) is implemented against a fake Jellyfin. A node reads its own Jellyfin as a non-administrator service user, publishes libraries with declared root paths, never exposes a protected library, and exchanges catalogs with the other members, honouring opt-outs and blocks at both ends. Confirming the adapter against a real Jellyfin 10.11.11 is still to do (conformance M-7). - Phase 3 (materialization and relay) is implemented. Other members' items appear in the destination's own Jellyfin libraries as generated `.strm` files with NFO metadata, subtitles, and posters, one item per film with a version per source, and play through a local relay that streams from the source over mutual TLS. Nothing protected, opted out, or blocked is written or playable.
- Progress is measured against [docs/conformance.md](docs/conformance.md); run `./scripts/verify.sh`.

## Running a node

```bash
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml exec jellymesh /jellymesh status
```

Found a group on one node, invite another, and approve it:

```bash
# On the founding node
jellymesh found family-and-friends
jellymesh invite                      # prints a short code, its address, and a QR payload

# On every node: see what the Jellyfin service user can see, and publish
jellymesh libraries
jellymesh publish <library-id> -root /media/movies

# On the joining node, which offers what it publishes
jellymesh join -address cedar.example.org:8443 -code XXXXX-XXXXX-XXXXX-XXXXX-XXXXX-XXXX

# On the owner's or any administrator's node
jellymesh requests
jellymesh approve <inviter-id> <invitation-id>
```

Then, once, in Jellyfin: add each folder `jellymesh generated` lists to the matching library (the Movies folder to your Movies library, the TV Shows folder to your TV library). Jellymesh cannot make Jellyfin rescan (assumption A-12), so set Jellyfin's "Scan media library" scheduled task to an interval you are happy with, such as hourly; new remote items appear at the next scan.

Afterwards, `jellymesh remote` lists what the other members publish, and `jellymesh optout <source-id> <library-id>` hides one of their libraries.

Jellymesh reads Jellyfin as a dedicated user that is not an administrator. Create it in Jellyfin, give it access to exactly the libraries you may publish, and set `JELLYMESH_JELLYFIN_USER` and `JELLYMESH_JELLYFIN_PASSWORD`. List libraries that must never be shared, such as a family library, in `JELLYMESH_PROTECTED_LIBRARIES`, by library ID.

Members reach each other at their advertised address, `JELLYMESH_PUBLIC_HOSTNAME`, over TCP. It must be reachable from outside your network, by a router port forward or by Tailscale Funnel in raw-TCP mode (assumption A-16); joining checks it. Media then prefers a direct UDP path between the two homes, opened by hole punching on `JELLYMESH_DIRECT_LISTEN_ADDR` (UDP 44843 by default; publish it from the container), and falls back to TCP by itself when no direct path can be opened. `jellymesh status` shows which path each peer uses.

In a container deployment, prefix each command with `docker compose exec jellymesh /jellymesh` in place of `jellymesh`. The admin API listens on loopback only.

## Background
- Cedar was inspected read-only and runs Jellyfin 10.11.11.0 in a host-network Docker container.
- Walnut has Docker and Docker Compose available but no Jellyfin deployment.
- No production server, account, library, or media has been modified.
- Phase 0 lab scaffolding is present under `lab/`.
- A Go Jellymesh Service skeleton is present under `cmd/jellymesh/` and `internal/`. Its dependencies are the pure-Go SQLite driver `modernc.org/sqlite` and `quic-go`, for direct UDP paths between members (assumption A-16).
- The policy core under `internal/policy/` models a node's own opt-in publication, automatic destination availability, explicit per-library opt-out, symmetric peer blocks, and the invitations it issues, consulting the replicated roster for membership.
- Watched state, resume positions, and favourites are Jellyfin's own, per local user. Jellyfin keeps a removed item's state and restores it when the work returns, so the materializer keeps each work's folder and identifiers stable for 90 days after it was last materialized (assumption A-14). The ledger model under `internal/history/` is a tested primitive that is not in that path.
- The materializer under `internal/materialize/` groups works across sources on shared TMDB, TVDB, or IMDb identifiers: a film is one item with a version per source, and a series is one show whose episodes each play from one source (assumptions A-11, A-13). A film held locally and remotely shows twice (A-15).
- The retention store under `internal/catalog/` keeps deletion metadata/artwork identity available for a configurable grace period, defaulting to 7 days.
- The sync policy under `internal/syncpolicy/` defaults to 5-minute health heartbeats and hourly incremental catalog sync with bounded backoff.
- The resource policy under `internal/limits/` defaults to one concurrent remote transcode per destination while leaving direct play unrestricted.
- The settings model under `internal/settings/` covers published libraries, source opt-outs, transcode limits, and sync-health visibility.
- The identity model under `internal/identity/` merges work only on strong media identity and leaves ambiguous items separate.
- The replicated group log under `internal/grouplog/` is the source of group membership: signed, hash-chained events that only the owner sequences, administrator decisions as signed proposals, and succession by signed attestations that fences the former owner. `internal/membership/` keeps it durable and derives transport trust from its roster. `internal/replication/` serves and fetches it between members over mutual TLS.
- Encrypted backup and restore under `internal/backup/` covers the node key, certificate, and a consistent database snapshot. It uses AES-256-GCM under a PBKDF2-SHA256 passphrase key, never overwrites an existing node, and holds a restored node back from sequencing until it has caught up.
- Audit events under `internal/audit/` are redacted twice before storage: detail only for allow-listed keys, and registered secrets such as the Jellyfin API key scrubbed from every field.
- Enrollment under `internal/enrollment/` admits a new node: an invitation by short code or QR code, redemption at the inviter over a connection pinned to the inviter's key, approval as a signed admission proposal, and a join that downloads the log anchored to its genesis. `internal/federation/` assembles the public listener, where only enrollment is reachable without a member's key.
- The owner-absence watch under `internal/group/` tracks this node's own view of the owner's availability: when a succession claim becomes eligible, and when a group with no owner or administrators dissolves.

## Documentation

- [Conformance criteria](docs/conformance.md) — the objective target, run with `./scripts/verify.sh`
- [Making a node reachable](docs/operator-reachability.md) — port forwards, Tailscale Funnel, direct paths, and troubleshooting
- [Runbook: compromised node key](docs/runbook-compromise-recovery.md)
- [Design specification](docs/design-spec.md)
- [Phase 0 validation plan](docs/phase-0-lab.md)
- [Follow-up decisions](docs/follow-up-decisions.md)
- [Discovery log](docs/discovery-log.md)
- [Plan review](docs/plan-review.md)
- [Phase 0 results](docs/phase-0-results.md)

## Working direction

The initial implementation direction is a Jellymesh Service beside each Jellyfin server. The Jellymesh Service synchronizes approved remote catalog data into normal Jellyfin-readable media references, while Jellyfin performs normal browsing, grouping, playback, and local user-state management. Remote roots will be integrated by Jellyfin collection type—Movies, TV Shows, and Music—rather than assuming one ordinary library can contain all three.

A permanent shared group key is not the authorization mechanism. Servers use pairwise trust, a group library pool, and per-source-library opt-out decisions. Every group has an owner and explicitly assigned administrators for invitations and membership state; the owner/admin control plane is not a media relay. Group membership is for server owners only; each joining server must publish at least one non-empty library. Blocking is a local pairwise media cut. Every library is private by default; Cedar’s `Family Movies` library must be explicitly protected from federation.
