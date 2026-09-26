# Jellymesh Plan Review

Status: Independent review of the planning-phase design
Date: 2026-09-24
Revision: r4, incorporating the second-pass annotation in section 10 and the measured results in docs/phase-0-results.md
Reviewed material: `README.md`, `docs/design-spec.md`, `docs/follow-up-decisions.md`, `docs/discovery-log.md`, `docs/phase-0-lab.md`, `lab/`, and the Go skeleton under `cmd/` and `internal/`

## 1. Summary

The plan is unusually disciplined for a planning-phase project. Private-by-default publication, the multi-layer enforcement list for protected libraries, identity-first deduplication, the refusal to write to Jellyfin's SQLite database, non-destructive sync on failure, and the honest prior-art review are all stronger than typical work at this stage.

The findings below are not cases of carelessness. They are places where a correct-sounding decision has a consequence that was not traced to the end. They are grouped by whether they can invalidate the architecture, and each notes why it matters rather than only what is wrong.

Findings that assert Jellyfin runtime behavior are marked as Phase 0 verification items rather than settled facts.

## 2. Architecture-invalidating risks

### A1. Integrating into existing libraries makes remote items inherit local scan settings

> **SUPERSEDED — measured and disproven, 2026-09-24.** The Phase 0 lab run measured **0 bytes** pulled from a remote `.strm` source during library scan, during a full metadata refresh with `replaceAllMetadata=true`, and during explicit runs of the `Generate Trickplay Images`, `Extract Chapter Images`, and `Keyframe Extractor` tasks, with all extraction options enabled. Jellyfin 10.11.11 does not probe remote HTTP `.strm` sources at scan time, so extraction has no media info to work from and skips them. A controlled comparison against an identical local file in the same library, which did probe and did yield two media streams, is in [phase-0-results.md](phase-0-results.md) section 2.
>
> The reasoning below is retained for the record. It was the finding ranked most likely to change the architecture, and it was wrong. Two smaller real findings replace it: a roughly 1 MB re-probe on **every** `PlaybackInfo` call, and the absence of any media metadata (`RunTimeTicks`, streams, correct `Size`) on remote items until first playback. Both belong in the security and product sections rather than here.

`design-spec.md` section 10 and follow-up decision 6 add generated roots into the destination's existing Movies, TV Shows, and Music libraries in order to obtain native collections and version grouping. A Jellyfin library's extraction settings apply to every path inside it, so remote `.strm` items inherit:

- ffprobe media-info probing on scan and on every metadata refresh;
- trickplay image extraction, which is enabled by default at the library level in recent Jellyfin releases;
- chapter image extraction;
- the local library's scheduled refresh cadence.

Trickplay and chapter extraction require ffmpeg to read the whole file. Pointed at a `.strm` served through the relay, one library scan can pull a peer's entire library across the WAN with no user action, on a timer. At twenty peers this is an unbounded transfer loop, not a bandwidth inconvenience.

Separate `Friends Movies`, `Friends TV`, and `Friends Music` libraries can disable extraction per library. The integrated model cannot, because local content needs those features enabled.

Why it matters: the sole justification for the integrated model is native version grouping and cross-server collections, and finding A2 indicates that justification may not hold. The plan is paying a severe and potentially dangerous cost for a benefit it has not yet verified it receives. `follow-up-decisions.md` currently lists the integrated-library choice as a usability question to revisit after Phase 0. It is a safety question and belongs in the Phase 0 exit gates.

Phase 0 action: measure whether adding `.strm` items to an existing library triggers probing, trickplay, and chapter extraction, and count the bytes that cross the WAN per scan.

### A2. Local-to-remote version grouping probably does not work as assumed

> **CONFIRMED, 2026-09-24.** Two copies of one work, identical `Tmdb: 999001`, same library, separate folders produced two separate cards. `POST /Videos/MergeVersions` was validated as a working mitigation, and the ` - SourceName` suffix surfaces as the version label. See [phase-0-results.md](phase-0-results.md) section 3.

Jellyfin groups alternate versions of a movie by files sharing a folder, not by provider ID. The generated layout places both peers' `.strm` files in one folder, so peer-to-peer merging is plausible. The local copy of the same movie lives in the local Movies library at a different path, in a different root. Two folders produce two movie entries even inside a single library.

The claim in `README.md` and `follow-up-decisions.md` that local and remote items can participate in the same native version relationships is therefore likely false by default. Jellyfin's `POST /Videos/MergeVersions` API can merge them programmatically, but Jellymesh would then own a merge graph that must be maintained and split on every tombstone, which is real state the design does not currently have.

Television is harder. A local series plus remote-only episodes is likely to appear as two series cards rather than one series with additional episodes. Whether episode-level alternate versions behave at all needs to be confirmed.

`phase-0-lab.md` step 16 only tests a second source version peer-to-peer. The local-plus-remote merge case, which is the case that motivated the integrated-library decision, is absent from the test matrix.

### A3. Music grouping contradicts the deduplication rule

> **CONFIRMED AND WORSE THAN STATED, 2026-09-24.** Measured: two artists, two albums, six tracks for a single three-track album shared by two sources. Adding `artist.nfo` and `album.nfo` with matching MusicBrainz identifiers changed nothing, and the remote album ended with no provider IDs at all. The **artist list itself duplicates**, so the damage is not confined to the album view. Recommendation is to cut music from v1. See [phase-0-results.md](phase-0-results.md) section 5.

Jellyfin has no alternate-versions concept for audio tracks. Twenty peers with overlapping music libraries produce duplicate albums and duplicate tracks in artist views.

The escape hatch in section 9 of the design specification, "record the exception rather than creating misleading duplicate cards," is not an escape hatch. The alternatives are duplicates or selecting a winning source, and selecting a winner directly violates the rule in section 10 that Jellymesh must not automatically prefer one source's version.

Why it matters: this is an unresolved internal contradiction sitting behind a Phase 0 gate that has no defined pass condition. Either define what acceptable music behavior means before the gate is run, or cut music from v1 and state that explicitly.

### A4. There is no bandwidth model, and the transcode limit measures the wrong resource

`internal/limits/policy.go` caps concurrent remote transcodes at one per destination. In the relay design the source never transcodes; per the JellyFed observation in `discovery-log.md`, the consumer performs the transcode work. Consequences:

- The scarce resource at the source is uplink bandwidth, not transcode slots. A typical residential uplink is 10 to 40 Mbps. A single 4K remux direct-play stream is 40 to 80 Mbps. Direct play is declared unrestricted in v1, so it will fail, and it will fail by saturating the source household's internet connection.
- Because the `.strm` URL is static, bitrate cannot be negotiated per session. The consumer must pull the full-bitrate original even when the end client needs 3 Mbps on a phone. Jellyfin's own remote-streaming model avoids this by transcoding at the source. This design structurally cannot.
- The transcode counter protects the wrong side. A destination can legitimately cap its own concurrent transcodes, because that limits its own CPU. It does nothing for the source, which has no visibility into whether a destination is transcoding and no control over it. The source's constraint is uplink bandwidth and needs its own control.

Missing: a per-peer bitrate or bandwidth cap at the source, and a decision on whether the source Jellymesh Service should request an already-transcoded stream from source Jellyfin, which introduces double-transcode and re-probing risk at the consumer. This is a v1 blocker rather than a tuning detail.

### A5. Catalog scale is unmodeled, and Phase 0 would pass on a toy library

> **CONFIRMED, 2026-09-24, with the emphasis corrected.** A completed 50,001-item run took 2.94 hours at 4.72 items/second, producing a 187 MB database at ~3.7 KB per item. That projects to ~23.5 hours and ~1.5 GB for 400,000 TV episodes. The real constraint is scan **time**, not disk: earlier in-run estimates of 52 KB/item and a degrading rate were both artifacts of extrapolating an early window and are retracted. See [phase-0-results.md](phase-0-results.md) section 4.

`discovery-log.md` computes request counts, roughly 4,560 heartbeats per hour for a twenty-node group, and concludes the traffic is acceptable. That is the tractable half of the problem.

The other half: twenty peers at 2,000 movies each is 40,000 generated folders, each with a `.strm`, an NFO, and artwork. Television at twenty peers, 200 series, 100 episodes each is roughly 400,000 episode `.strm` and NFO files. That is on the order of one million small files per node, plus tens of gigabytes of cached artwork, plus a Jellyfin SQLite database that must index all of it. Combined with A1's per-item probing, the first scan may never complete.

There is also a product consequence. The local Movies view becomes largely other people's content, and "Recently Added," "Next Up," and search are flooded by peer syncs. Nothing in the plan addresses suppressing remote items from those surfaces or offering a local-only browse path.

The Phase 0 lab contains a handful of synthetic items. It will pass every current test and reveal nothing about the failure mode that will actually occur. A synthetic scale test of 50,000 to 100,000 generated items should run before the presentation features, because it is cheap to generate and is the test most likely to change the architecture.

Related: a new node joining a twenty-node group performs nineteen full initial syncs. First-join is measured in hours or days and tens of gigabytes, with no staging or prioritization in the plan.

## 3. Security and trust model holes

### B1. Owner succession is a consensus problem with no consensus algorithm

> **RESOLVED, 2026-09-24.** `Advance` no longer promotes on a local timer; it reports that a claim has become *eligible*. `ClaimOwnership` installs an owner only against absence attestations from a strict majority of non-owner members, rejecting self-attestation, owner attestation, non-members, duplicates, and observations too recent to support the claim. The confirmed rule about who succeeds is unchanged. Implemented in `internal/group/roles.go` with eight tests including three adversarial cases.
>
> **REOPENED IN PART, 2026-09-26.** The attestations are not signed, so a claimant can write the quorum itself (C-PO-14), and a returning former owner is not fenced (C-PO-15). The paragraph below on conflicting signed membership events was not addressed by the succession change and remains open as C-PO-17.
>
> **RESOLVED, 2026-09-26.** Superseded by the replicated group log (design-spec section 8, "Group state and event replication"; `internal/grouplog`). Only the owner of an epoch sequences, so there are no conflicting events to resolve; attestations are signed; succession opens a new epoch that fences the former owner. `ClaimOwnership` and `ApplyVerifiedRevocation`, named below, no longer exist.

`discovery-log.md` lists split-brain ownership as a risk and does not resolve it. `internal/group/roles.go:204` makes the gap concrete: `Advance()` is driven purely by the local clock and the local view of the administrator set.

Two nodes on opposite sides of a partition will observe owner unavailability at different moments and can promote different successors. A blocked peer receives no heartbeats from the peer it blocked, so its availability view is permanently wrong by design. Two resulting owners then sign conflicting admissions and revocations, and there is no rule for resolving conflicting signed membership events. `ApplyVerifiedRevocation` accepts the highest sequence number, which both owners will independently increment.

Unavailability is also attested by nobody. A malicious member can declare the owner absent to itself and claim succession.

Fix direction: require M-of-N administrator co-signature for succession; or have a claimant broadcast a succession claim carrying absence attestations from a quorum of members, with a settling window and a deterministic tie-break; or remove automatic succession entirely. For a twenty-person friend group, manual recovery is a legitimate answer that deletes an entire class of bug.

### B2. Transitive invitations grant library access without the source's evaluation

Under section 8 of the design specification, any active member creates an invitation, an owner or administrator approves it, step 8 establishes pairwise trust with approved members, and published libraries auto-accept at destinations.

The net effect is that a node the source never evaluated receives the source's catalog by default, and the only recourse is to notice and block afterward. Social trust does not survive transitive growth to twenty nodes.

Combined with A5's flood problem, auto-accept appears to be the wrong default in both directions. At minimum, add a per-source "new group members require my approval" control and a per-peer "manual accept" mode on the destination side. Neither exists in `internal/policy` or `internal/settings` today. Opt-out is per-exact-library only, which means the privacy control must be exercised once per source library, reactively, indefinitely.

### B3. The Jellyfin admin API key is the real blast radius

To enumerate libraries and stream arbitrary items, the source Jellymesh Service requires a Jellyfin administrator API key stored in the sidecar. Compromise of Jellymesh therefore grants full Jellyfin administration on that host, including `Family Movies`, regardless of the seven enforcement layers in section 7, because those layers are Jellymesh's own code.

Recommendation: create a dedicated non-administrator Jellyfin service user whose library access is restricted to exactly the published libraries, and have Jellymesh authenticate as that user. The guarantee that `Family Movies` is never exposed then rests on Jellyfin's own permission system rather than on Jellymesh correctness alone. This converts the most important guarantee in the product from "the checks were written correctly in seven places" to "the credential cannot see it," which is a categorically stronger property and is inexpensive to implement.

### B4. Library-to-item attribution goes stale, and paths can overlap

Two concrete leak paths for the protected-library guarantee:

- Jellyfin items can be moved between libraries. If the stream endpoint authorizes against the catalog snapshot's library attribution rather than live Jellyfin state, a moved item retains its previous published attribution. Library ownership must be re-resolved at stream time from live Jellyfin state, and the specification should say so.
- Jellyfin permits a path to appear in more than one library, and symlinks or hardlinks can bridge roots. A normalized path fingerprint check, as described in rule 6 of section 7, can pass while the file remains reachable through the published library. Denial should be evaluated against the resolved real path after symlink resolution, against any protected root. That is the invariant worth testing.

### B5. Peer authentication is unspecified and replay protection is absent

> **RESOLVED, 2026-09-24.** Mutual TLS with the node's Ed25519 key as the client certificate key; authorization by key fingerprint against the pairwise trust record, never by hostname. Replay protection comes from the TLS session, which avoids adding clock dependence on top of B9. Recorded in `design-spec.md` section 8, "Transport authentication". **Key rotation remains open** and is tracked there.

Section 8 establishes Ed25519 node identities and correctly separates "TLS proves hostname" from "Jellymesh approves peer." Nothing states how a request is bound to a node identity: mutual TLS with node keys, signed request envelopes, or pairwise session tokens. No nonce or timestamp replay protection appears anywhere in the document.

Key rotation appears only as a Phase 5 drill with no protocol. With no central directory, a peer learns a new key through a rotation event signed by the old key, gossiped through the mesh, with a separate path for compromise where the old key cannot sign. Owner key rotation is harder still, for the reasons in B1.

### B6. `.strm` contents and token leakage

> **CONFIRMED AND MORE SEVERE, 2026-09-24.** A non-administrator local user read the full peer origin URL through both `/Items?Fields=MediaSources` and `/Items/{id}/PlaybackInfo`. Separately, a user restricted to zero library access streamed 2 MB of peer media through `/Videos/{id}/stream`: Jellyfin's library ACL gates metadata but not the raw stream route. See [phase-0-results.md](phase-0-results.md) section 6.

The specification requires no arbitrary remote URL proxying and protection against token leakage, but never states what a `.strm` file actually contains. It must hold a local URL to the local relay carrying an opaque, unguessable, non-authenticating item identifier. It must never hold a peer URL or a bearer token. A token placed in a `.strm` propagates into Jellyfin's `Path` column, into the generated NFO, into logs, and becomes reachable through Jellyfin's MediaSources API, which non-administrator users can query for items they can see.

Two consequences follow. The relay listener needs its own local authorization, because a loopback bind is insufficient once Jellyfin and Jellymesh run in separate container network namespaces. The public federation listener and the local relay listener must be separate sockets so that one misconfiguration cannot expose the relay.

`internal/config/config.go:24` defines a single `ListenAddress` defaulting to `127.0.0.1:8090`, which a sidecar container cannot serve to Jellyfin in another namespace, and which provides no separation between the two roles.

### B7. Unauthenticated readiness endpoint discloses node identity

`internal/httpapi/server.go:53` returns `NodeName` with no authentication, and `/healthz` returns federation state. On a public endpoint this is free reconnaissance. Bind these to the local listener or require peer authentication.

### B8. Legal exposure and abuse handling are absent

The planning material contains no treatment of copyright exposure, ISP terms of service, or the handling of a peer that publishes genuinely illegal content. Note that peers' metadata and artwork are auto-ingested to local disk, which places third-party content from an unvetted source onto every member's storage. There is no reporting, quarantine, or review-before-materialize path.

This is a gap flag rather than a legal judgment. A plan this meticulous about policy has a conspicuous blank where the risk that most often ends projects of this shape resides. At minimum: a group-size cap, the approval defaults from B2, and a written statement of what participants are agreeing to.

### B9. All timers run on local wall-clock

The 15-day, 7-day, and 5-day windows are local-clock decisions in `group.Advance`, `policy.AdvanceCoordinatorHealth`, and `RetentionRecord.ExpiresAt`. A skewed or manipulated clock can expire invitations early, hold them late, or trigger succession. `phase-0-lab.md` tests clock drift for synchronization only. Signed events should carry issuer timestamps, and local countdowns should use monotonic elapsed time.

## 4. Policy model gaps

### C1. Publication is per-library, but Jellyfin libraries are mutable

Rule 7 in section 7 covers renamed and recreated libraries. It does not cover the common case: the owner adds a path to an already-published library. Everything beneath the new path federates silently. The same applies to items moved into a published library.

Recommended fix: record the set of root paths at publication time. Any change to that set moves the library into a "needs re-confirmation" state and pauses publication. This is a small change that closes the most likely real-world privacy accident.

Relatedly, new content federates automatically with no notification, which is a deliberate decision. The result is that a user never encounters a moment reading "47 new items were just shared with 19 people." An audit log entry and a settings-page counter would materially change how safe the system feels without changing the decision.

### C2. `Family Movies` is special-cased in a way that invites a name check

The specification correctly states that name-only matching is insufficient, then devotes a subsection to elevating one named library to an architectural rule. The invariant that should be built and tested is the general one: default-deny everything not on an explicit allowlist keyed by stable library identity plus resolved path fingerprint.

Rewording this as a protected-library set, with `Family Movies` as its first entry, causes implementers to build and test the general property rather than the instance.

### C3. Ejection has no take-back, and this belongs in the product surface

Section 8 correctly notes that ejection cannot erase what a member already downloaded. Stated in product terms: any peer that ever saw a source's catalog retains a permanent copy of that library listing. Users need to understand this in the publication flow, before they publish, not only in the specification.

### C5. Jellyfin library permissions are not a playback boundary

> **MEASURED, 2026-09-24.** Confirmed product decision 4 states that normal Jellyfin library permissions decide which local users may access remote media. Measurement shows that holds for browsing only. A user with `EnableAllFolders: false` and an empty `EnabledFolders` was correctly denied enumeration, item fetch, and `PlaybackInfo`, but successfully streamed the item through `/Videos/{id}/stream` given its ID.

The decision should be narrowed to **browse** access, and the design should state that the relay cannot assume Jellyfin authorized a request before forwarding it. Because the relay is invoked by Jellyfin rather than by the end user, it also cannot determine which local user triggered playback, so per-user policy cannot be enforced there either. If per-user enforcement is a requirement, it needs a mechanism the current design does not have.

This does not weaken the protected-library guarantee: a protected library is never materialized and so has no item ID to request. The exposure is to published remote media whose IDs are known, and IDs appear not to be path-derived, so the realistic case is a leaked rather than a guessed identifier.

### C4. Subtitles, trickplay, and extras are absent from the materialization design

> **CONFIRMED, 2026-09-24.** An external `.en.srt` beside a remote `.strm` attached correctly as `subrip`/`eng`/`IsExternal=true`. Subtitles cannot be relayed lazily, so subtitle content must be copied to the destination. The generated layout in `design-spec.md` section 10 needs a place for it. See [phase-0-results.md](phase-0-results.md) section 6.

The specification requires protecting stream and subtitle endpoints, so a subtitle endpoint exists, yet the generated layout in section 10 contains no subtitle files. External subtitle files cannot be relayed lazily, because Jellyfin needs them present on disk. Subtitle content must therefore be copied, which is a real, small exception to the rule that Jellymesh does not copy media files and should be stated as such. The same question applies to chapter images, theme songs, and extras.

## 5. Contradictions between the documents and the skeleton

These are inexpensive to correct now and expensive later, because the skeleton establishes the patterns that subsequent code will follow.

### D1. Two divergent membership models

`internal/policy` implements a single-coordinator model with `IsCoordinator`, `TransferCoordinator`, `ErrNotCoordinator`, and its own dissolution state machine. `internal/group` implements owner-plus-administrators with succession. Both own `Members`, both define `DissolutionNotificationInterval`, and both track dissolution. They will drift. Collapse them into one membership model before more code lands.

### D2. Administrators cannot approve invitations

`internal/policy/policy.go:227` and `internal/policy/policy.go:251` gate `ApproveInvitation` and `DenyInvitation` on `IsCoordinator`. The specification states that an owner or administrator approves or denies. This directly contradicts a confirmed decision.

### D3. Rejoin is structurally impossible

`EjectMember` calls `removePublishedLibraries` at `internal/policy/policy.go:391`, deleting the ejected member's publications. `ApplyVerifiedAdmission` calls `ValidateMemberAdmission` at `internal/policy/policy.go:444`, which requires at least one publication. An ejected member therefore holds zero publications and can never satisfy readmission.

Step 6 of the enrollment flow implies a candidate publication set separate from the live group pool. The code has no such concept. `phase-0-lab.md` step 23 tests exactly this path and would fail.

### D4. Marking the coordinator available can dissolve the group

At `internal/policy/policy.go:299` through `:314`, calling `MarkCoordinatorAvailable` after the deadline sets `Status = GroupDissolved` and returns an error. A recovery call that destroys the group as a side effect is a hazard, and it is reachable from a heartbeat handler.

### D5. The history merge silently undoes a deliberate un-watch

At `internal/history/history.go:90`, `local.Played = local.Played || preserved.Played`. If a user explicitly marks an item unwatched and the item later returns from another source, the ledger re-marks it watched. This violates the rule that existing local state is authoritative, and it produces the failure users describe as "Jellyfin keeps marking things watched."

Fix: compare `UpdatedAt` before combining `Played`, or record an explicit un-watch tombstone.

### D6. The work identity model cannot address episodes

`internal/identity/identity.go` models a flat type, provider, and identifier triple. Episode-level provider identifiers are inconsistently present in real libraries, so a ledger keyed on strong identity will fail to restore most television progress, which is precisely where resume positions matter most.

Recommendation: use series provider identifier plus season plus episode number as the television work key. It is safe enough and offers far higher coverage. Music has the same problem with MusicBrainz recording identifiers.

Separately, `NewWorkIdentity` does not canonicalize provider names. `Key()` lowercases, so `TMDB` and `tmdb` match, but `themoviedb` and `tmdb` do not. Cross-server deduplication is the central premise of the product and requires a provider-name canonicalization table.

### D7. Backoff has no jitter

`internal/syncpolicy/policy.go:61` implements pure exponential backoff. The specification requires jitter in three separate places, specifically to prevent synchronized bursts across the mesh. With twenty nodes recovering from a shared outage, jitter is the difference between recovery and a thundering herd.

### D8. Opt-outs cannot be set pre-emptively

`internal/policy/policy.go:518` requires a library to be currently published before an opt-out can be recorded. A destination cannot declare "I never want anything from peer X" and cannot opt out of a library before it appears. Combined with B2, the destination has no proactive control at all.

The retain-on-unpublish behavior is correct and matches the rule that unblocking preserves opt-outs, but it is undocumented and invisible in the interface while the library is unpublished.

### D9. Inconsistent clock injection

`group.Advance(now)` and `RecordDeletion(..., deletedAt)` accept an injected time, while `policy.CreateInvitation` and `policy.SetOptOut` call `time.Now()` internally. `phase-0-lab.md` steps 19 and 20 require simulating 15-day timeouts, which is impossible without injection throughout. Make timers overridable for tests.

### D10. In-memory maps will not hold a real catalog

Every store is a Go map with full-map scans, including `RetentionStore.Expire`. At the scale described in A5 this requires an embedded database with indexes. The specification calls for persistent state but does not select a store, and the skeleton is quietly establishing the wrong shape.

### D11. Misleading error and stuck state machine

`group.Eject` at `internal/group/roles.go:131` returns `ErrNotMember` when the actor lacks administrator rights, which misidentifies the failure. `MarkOwnerAvailable` at `internal/group/roles.go:194` returns `ErrOwnerSuccessionClosed` past the deadline without changing `Status`, leaving the state machine in `succession_pending` until `Advance` runs.

### D12. Configuration does not reflect the architecture

> **RESOLVED, 2026-09-24.** `internal/config` now carries three separate listeners (public federation, loopback relay, loopback admin), node key and certificate paths, public hostname, Jellyfin base URL and API key, and the generated root. Listener collisions are refused, and enabling federation without a hostname or Jellyfin key fails at load. Eleven tests.

`internal/config` defines no Jellyfin URL, no Jellyfin credential, no public hostname, no TLS configuration, and no separation between the public federation listener and the local relay listener.

## 6. Gaps in the Phase 0 test matrix

Phase 0 is the go/no-go gate, so gaps here are the most consequential gaps in the plan. Missing tests, in approximate priority order:

1. Scale: 50,000 to 100,000 generated items, measuring scan time, database growth, memory, and artwork disk usage.
2. Extraction behavior: whether adding `.strm` items to an existing library triggers probing, trickplay, and chapter extraction, and how many bytes cross the WAN per scan. See A1.
3. Local-plus-remote version merge, the case that justified the integrated-library decision. See A2.
4. Bandwidth ceiling: a 4K source over a throttled 10 Mbps uplink, and two peers pulling simultaneously.
5. Token and path leakage: query MediaSources as a non-administrator local user and confirm that no relay token or peer URL is visible.
6. Library path mutation after publication, confirming the library fails closed. See C1.
7. Split-brain succession: partition two nodes, allow both to promote, and observe the conflict. See B1.
8. Recovery storm: a peer offline for 16 days returns, measuring full-resync cost across the mesh.
9. Jellyfin version skew: twenty independent homes will run different Jellyfin minor versions simultaneously. Protocol versioning is addressed; Jellyfin version skew is never discussed. Define a minimum-version gate and a compatibility matrix.
10. Seek storms: Roku in particular re-requests ranges aggressively. Validate relay behavior under that pattern.
11. Clock skew applied to membership events, not only to synchronization. See B9.
12. Rescan cost under rolling tombstones: `POST /Library/Refresh` is coarse, so per-tombstone rescans across twenty peers approach continuous scanning. This needs debounce, batching, and a named API strategy.

## 7. Product and operational gaps

### F1. No visibility into who is streaming a source's media

A source owner cannot list active remote sessions, cannot terminate one, and cannot cap a specific peer. For a system built on sharing with friends, this is the most basic trust affordance and it is entirely absent. Relay pulls appear in Jellyfin as a single service user, so Jellyfin's own session view does not compensate.

Recommendation: per-peer active-stream list, a kill switch, and a per-peer bandwidth cap in the Jellymesh settings surface.

### F2. The settings page contradicts the sidecar decision, and has no authentication model

The specification repeatedly refers to "the Jellymesh settings page" as though it were a Jellyfin dashboard page, while follow-up decision 21 specifies a sidecar container that does not modify Jellyfin. A sidecar cannot add a dashboard page without being a plugin, so the settings page is a second web interface.

Nothing in the planning material specifies how anyone authenticates to that interface, despite it controlling what is published to the public internet. The strongest available answer is to validate a Jellyfin user token against Jellyfin's API and require administrator status. That yields one identity, no new password, and no new attack surface.

### F3. The stated persona does not match the deployment requirements

Each node requires a public hostname, a valid TLS certificate, port forwarding, and Docker Compose. That is a substantial gap against the goal that normal people can run the service on a home server. Either bundle the hard parts, for example a reverse proxy with automatic certificates plus DNS guidance, or adjust the stated persona.

### F4. Offline is the normal state, and its user experience is an error dialog

Twenty home servers means several are down at any moment. The specification accepts the failed-playback path and lists source suppression as optional. At twenty peers this will be the dominant user complaint. Measure actual peer uptime during the pilot before committing to keeping unavailable cards visible.

### F5. The reciprocity rule is decorative

Admission requires one non-empty published library, which a single file satisfies. A consuming-only member can take group bandwidth while contributing nothing of substance. Either make reciprocity real through a ratio, a minimum item count, or explicit group policy, or remove the rule rather than implying a fairness property it does not deliver.

## 8. What the plan gets right

Worth preserving explicitly, because several of these are load-bearing and easy to lose during a refactor:

- Private-by-default publication with explicit opt-in.
- The refusal to write to Jellyfin's SQLite database.
- Separating trust, publication, opt-out, membership, and blocking into five independent concepts. Most designs collapse these and regret it.
- Identity-first deduplication with an explicit refusal to merge on title and year.
- Tombstones as explicit events, never inferred from a failed heartbeat, and failed syncs never becoming deletions.
- Separating the history ledger from generated artifacts, which is the right insight even with D5 and D6 outstanding.
- Honest non-goals, including the absence of a guaranteed greyed-out provider state.
- The prior-art review, in particular the note that JellyFed declares no license and must not be copied.
- Phase 0 as a genuine go/no-go rather than a formality.

## 9. Recommended sequence

The work splits into two tracks that do not depend on each other and should run in parallel. Track 1 cannot be resolved by deliberation, only by measurement. Track 2 is unblocked today and gets more expensive the longer it waits.

### Track 1: measurements that may change the architecture

Nothing here is settled by discussion. Run the lab.

1. Run the extraction test (A1) and the scale test (A5) before anything else. Both are inexpensive, and together they are the most likely to change the architecture. If probing, trickplay, or chapter extraction fire on remote `.strm` items, the integrated-library decision is dead and separate `Friends` libraries become mandatory.
2. Test local-plus-remote version merge (A2). If it fails, the integrated model has no remaining justification and A1's risk disappears along with it.
3. Measure the bandwidth ceiling, then decide the bandwidth model (A4), including source-side bitrate caps and whether the source transcodes. This is a v1 blocker.

### Track 2: decisions and repairs that are unblocked now

None of this waits on a lab result, and the code is small enough today that the refactors are cheap.

4. Switch to a restricted Jellyfin service user (B3). This is the highest security value per unit of work in the list.
5. Reconcile `internal/policy` and `internal/group` into one membership model (D1 through D4), while the affected code is still roughly 350 lines.
6. Repair the defects that have no design dependency: the un-watch regression (D5), missing backoff jitter (D7), and inconsistent clock injection (D9). D9 in particular gates the Phase 0 succession and dissolution tests, which cannot otherwise be run without waiting fifteen real days.
7. Resolve or remove automatic owner succession (B1). For a twenty-member friend group, manual recovery is a legitimate answer that removes a hard problem.
8. Add pre-emptive and per-peer opt-out controls plus new-member approval defaults (B2, D8).
9. Write the peer authentication and replay-protection section (B5) and specify `.strm` file contents (B6).

### Scope of the two-node MVP gate

The two-node vertical slice should not begin until Track 1 has produced results and the Track 2 protocol decisions (B1, B2, B5) are either settled or explicitly recorded as Phase 0 exit criteria. That gate does not extend to Track 2's repairs, which should proceed immediately and independently.

## 10. Second-pass annotation

Reviewer: Space Bunny (OpenCode)
Date: 2026-09-24
Disposition: concurrence. No finding was disputed and no new finding was added. The A4 correction raised below has been folded into section 2, and section 9 was restructured into parallel tracks in response to the sequencing note. The text below is the annotator's, unedited.

I agree that this review found several real pre-implementation blockers rather than documentation nits. The strongest findings are the divergent membership models (D1–D4), the Jellyfin service-user blast radius (B3), the unspecified peer authentication and relay-token boundary (B5–B7), mutable library publication (C1), the local-history merge bug (D5), the missing jitter and clock injection (D7/D9), and the inadequate configuration/state model (D10/D12).

I agree with A1–A3 as high-priority Phase 0 experiments, but I would not treat their Jellyfin runtime behavior as proven until the lab measures it. The integrated-library model should be considered conditional on extraction traffic, local-plus-remote grouping, and catalog-scale results. A4 is also a v1 blocker: the destination-side transcode cap may be enforceable locally, but it does not protect the source uplink, so the bandwidth model still needs an explicit source-side control.

The review also identifies decisions that should be reopened explicitly: whether music belongs in the MVP, whether automatic owner succession should be replaced by manual recovery or quorum/consensus, and whether sources must approve new group members instead of auto-accepting every published library. These are product/protocol decisions, not merely implementation details.

I agree with B2: source-side new-member approval and destination-side pre-emptive peer/library controls are needed before transitive group growth. The current auto-accept behavior is safe only for a very small trusted group and should not be treated as the long-term default without a deliberate policy.

I agree that the sidecar settings surface needs its own authentication and network boundary. It cannot be a Jellyfin dashboard page without a plugin, so the design should call it a separate Jellymesh administration interface and specify how a local Jellyfin administrator identity authenticates to it.

The review’s recommended sequence is sound: run extraction, scale, local-plus-remote grouping, and bandwidth tests first; then resolve music scope, service-user permissions, succession, membership, peer authentication, and persistent storage. I would not start the two-node MVP implementation until these decisions are either settled or explicitly marked as Phase 0 exit criteria.

I found no reason to discard the sidecar approach itself. The review identifies risks in its current materialization and trust design, not a reason to abandon the deployment model. The sidecar can remain the chosen architecture if the sidecar’s relay, public federation, local settings, credential, persistence, and media-fetch boundaries are specified and tested before implementation.
