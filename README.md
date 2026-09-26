# Jellymesh

Jellymesh is a planned federation layer for independent Jellyfin servers. Each home server can publish selected libraries to approved peer servers while continuing to use ordinary Jellyfin clients and local Jellyfin user accounts.

## Current status

- Planning and feasibility phase.
- Next implementation target: a two-node vertical slice covering invite/approval, one published library, catalog materialization, playback, and health sync.
- Cedar was inspected read-only and runs Jellyfin 10.11.11.0 in a host-network Docker container.
- Walnut has Docker and Docker Compose available but no Jellyfin deployment.
- No production server, account, library, or media has been modified.
- Phase 0 lab scaffolding is present under `lab/`.
- A Go Jellymesh Service skeleton is present under `cmd/jellymesh/` and `internal/`. Its only dependency is the pure-Go SQLite driver `modernc.org/sqlite`.
- The policy core under `internal/policy/` models a node's own opt-in publication, automatic destination availability, explicit per-library opt-out, symmetric peer blocks, and the invitations it issues, consulting the replicated roster for membership.
- The history ledger model under `internal/history/` preserves per-user watched state, play counts, and resume positions across generated-content cleanup.
- The retention store under `internal/catalog/` keeps deletion metadata/artwork identity available for a configurable grace period, defaulting to 7 days.
- The sync policy under `internal/syncpolicy/` defaults to 5-minute health heartbeats and hourly incremental catalog sync with bounded backoff.
- The resource policy under `internal/limits/` defaults to one concurrent remote transcode per destination while leaving direct play unrestricted.
- The settings model under `internal/settings/` covers published libraries, source opt-outs, transcode limits, and sync-health visibility.
- The identity model under `internal/identity/` merges work only on strong media identity and leaves ambiguous items separate.
- The replicated group log under `internal/grouplog/` is the source of group membership: signed, hash-chained events that only the owner sequences, administrator decisions as signed proposals, and succession by signed attestations that fences the former owner. `internal/membership/` keeps it durable and derives transport trust from its roster.
- The owner-absence watch under `internal/group/` tracks this node's own view of the owner's availability: when a succession claim becomes eligible, and when a group with no owner or administrators dissolves.

## Documentation

- [Conformance criteria](docs/conformance.md) — the objective target, run with `./scripts/verify.sh`
- [Design specification](docs/design-spec.md)
- [Phase 0 validation plan](docs/phase-0-lab.md)
- [Follow-up decisions](docs/follow-up-decisions.md)
- [Discovery log](docs/discovery-log.md)
- [Plan review](docs/plan-review.md)
- [Phase 0 results](docs/phase-0-results.md)

## Working direction

The initial implementation direction is a Jellymesh Service beside each Jellyfin server. The Jellymesh Service synchronizes approved remote catalog data into normal Jellyfin-readable media references, while Jellyfin performs normal browsing, grouping, playback, and local user-state management. Remote roots will be integrated by Jellyfin collection type—Movies, TV Shows, and Music—rather than assuming one ordinary library can contain all three.

A permanent shared group key is not the authorization mechanism. Servers use pairwise trust, a group library pool, and per-source-library opt-out decisions. Every group has an owner and explicitly assigned administrators for invitations and membership state; the owner/admin control plane is not a media relay. Group membership is for server owners only; each joining server must publish at least one non-empty library. Blocking is a local pairwise media cut. Every library is private by default; Cedar’s `Family Movies` library must be explicitly protected from federation.
