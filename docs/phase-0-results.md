# Phase 0 Lab Results: Run 1

Status: Measured results from the first Phase 0 lab run
Date: 2026-09-24
Environment: Two disposable Jellyfin 10.11.11 containers on Walnut, plus a byte-accounting origin service standing in for a peer Jellymesh relay
Image digest: `sha256:aefb67e6a7ff1debdd154a78a7bbb780fd0c873d8639210a7f6a2016ad2b35db` (identical to Cedar's production image)
Cedar: not contacted. No production server, account, library, or media was touched.

## 1. Headline

The run tested every architecture-invalidating finding from [plan-review.md](plan-review.md), then completed the remaining Phase 0 coverage.

| Finding | Verdict |
|---|---|
| A1: remote items inherit local extraction and pull whole files over the WAN | **Disproven.** 0 bytes on every scan and extraction path. Replaced by two smaller real findings. |
| A2: local and remote copies will not version-group | **Confirmed.** `MergeVersions` validated as a working mitigation. |
| A3: music grouping contradicts the dedupe rule | **Confirmed, and worse.** The artist list itself duplicates, and NFO metadata does not repair it. |
| A5: catalog scale is unmodeled | **Confirmed.** 50,001 items in 2.94 hours. The constraint is time, not disk. |

Phase 0 also covered subtitles (work, and must be copied), source deletion (clean), per-user progress isolation (confirmed), and the privacy surface, where the most consequential new finding of the run appeared: **Jellyfin's library permissions do not gate the raw stream endpoint**.

Two of the night's own projections were wrong and are recorded as retractions in section 4, both from extrapolating an early window rather than a completed run.

Decisions taken and implemented: the membership model reconciled onto owner/administrator (D1-D4), succession converted from a local timer to quorum-attested claims (B1), and the configuration model rebuilt around the mutual-TLS decision (D12). The persistence layer (D10) was deliberately left for ratification; see section 10.

## 2. A1: disproven as stated

### What was claimed

That adding remote `.strm` items to an existing Jellyfin library causes them to inherit that library's ffprobe, trickplay, and chapter-image extraction settings, and that extraction would read whole files across the WAN on a timer.

### What was measured

A 115,526,100-byte (110 MB, 180 s, 5.1 Mbps) synthetic MKV was served by a byte-accounting HTTP origin. A `.strm` pointing at it was placed in a Jellyfin movie library.

| Operation | Bytes pulled from origin |
|---|---|
| Library scan, extraction disabled (API default) | **0** |
| Library scan, `EnableTrickplayImageExtraction` + `ExtractTrickplayImagesDuringLibraryScan` + chapter extraction all `true` | **0** |
| Full metadata refresh, `replaceAllMetadata=true` | **0** |
| `Generate Trickplay Images` scheduled task, run explicitly | **0** |
| `Extract Chapter Images` scheduled task, run explicitly | **0** |
| `Keyframe Extractor` scheduled task, run explicitly | **0** |

All three extraction tasks returned to `Idle` without reading a byte.

### Controlled comparison

The same media file was placed locally in the same library with identical settings and scanned in the same pass:

| | Remote `.strm` | Local file |
|---|---|---|
| `RunTimeTicks` | `None` | `1800230000` |
| `Protocol` | `Http` | `File` |
| `Container` | `strm` | `mkv` |
| `Size` | `75` (the `.strm` file itself) | `115526100` |
| `MediaStreams` | **0** | **2** |

Jellyfin probes local files during a scan and does not probe remote HTTP `.strm` sources at all. Because the remote item therefore has no media streams and no runtime, the extraction tasks have nothing to operate on and skip it. The causal chain is: no scan-time probe, so no media info, so no extraction.

### Consequence for the design

The argument in the review that the integrated-library model is unsafe because of inherited extraction does not hold on 10.11.11. The integrated-library decision should be re-evaluated on its remaining merits (section 3), not on transfer risk.

### What replaces A1

**A1-a. Probing is deferred to playback, and repeats on every request.**

| Operation | Bytes pulled |
|---|---|
| First `POST /Items/{id}/PlaybackInfo` | 786,432 |
| Second `PlaybackInfo` | 983,040 |
| Third `PlaybackInfo` | 1,048,576 |

Roughly 1 MB leaves the source's uplink on **every** `PlaybackInfo` call, not only the first. Clients commonly call `PlaybackInfo` when an item detail page is opened, not only when playback starts. Probed values do persist onto the item, so the repeat cost is pure waste. This needs a source-side cache or a supplied-media-info path.

The probe issues an open-ended `Range: bytes=0-` and the connection is closed after roughly 1 MB. A relay must therefore handle client-initiated aborts cleanly; the specification already requires cancellation and backpressure, and this confirms the requirement is load-bearing rather than theoretical.

**A1-b. Remote items carry no media metadata until first playback.**

Before any playback the item has `RunTimeTicks=None`, zero media streams, and `Size=75`. That breaks duration display, any progress or resume percentage that is computed from runtime, and quality or codec labels in version selection. `Size` remains `75` even after a successful probe, which is simply wrong and may affect clients that use it.

This is a real obstacle to the specification's promises about local progress and about showing multiple labelled versions, and it is not addressed anywhere in the current design.

## 3. A2: confirmed

Two copies of the same work were placed in one library, in separate folders, both resolving to the identical provider identity `Tmdb: 999001`:

```text
/media/local-movies/Sentinel Probe (2019) [tmdbid-999001]/... - Local.mkv
/media/remote-movies/Sentinel Probe (2019) [tmdbid-999001]/... - PeerOrigin.strm
```

Result after a scan: **two separate movie cards**, each with one media source, despite identical provider IDs in the same library.

Jellyfin groups alternate versions by folder, not by provider identity. The review's A2 is confirmed, and the claim in `README.md` and `follow-up-decisions.md` that local and remote items participate in the same native version relationships is false by default.

### Mitigation validated

`POST /Videos/MergeVersions?Ids=<a>,<b>` returned 204 and produced the desired result:

```text
1 card, 2 media sources
  - Http  name='Sentinel Probe (2019) [tmdbid-999001] - PeerOrigin'  runtime=1800230000
  - File  name='Sentinel Probe (2019) [tmdbid-999001] - Local'       runtime=1800230000
```

Two useful secondary results: the merge is achievable through a supported API, and the ` - SourceName` filename suffix surfaces as the version label, which is exactly the source-labelling approach the specification proposes.

The cost is the one the review predicted: Jellymesh must actively drive merges and maintain the merge graph, splitting it on every tombstone. That is real state the design does not currently model.

## 4. A5: confirmed, and the constraint is time rather than disk

50,000 synthetic movie items (100,000 files: one `.strm` and one `movie.nfo` each) were generated in 6 seconds and scanned to completion.

### Completed run

| Measure | Value |
|---|---|
| Items indexed | 50,001 |
| Wall-clock scan time | 10,588 s (**2.94 hours**) |
| Mean index rate | **4.72 items/second** |
| Corpus on disk | 391 MB |
| Final Jellyfin DB | 186.7 MB |
| Marginal DB cost | **~3.7 KB per item** |
| Container CPU during scan | 119.7% sustained |
| Container memory during scan | 615 MB |

### Growth curve

| t (s) | indexed | DB (MB) | bytes/item | items/s |
|---|---|---|---|---|
| 1,362 | 5,062 | 160.9 | 31,685 | 3.72 |
| 2,893 | 12,104 | 152.3 | 12,541 | 4.18 |
| 4,576 | 20,673 | 163.0 | 7,863 | 4.52 |
| 7,101 | 33,288 | 170.3 | 5,101 | 4.69 |
| 9,626 | 45,541 | 185.6 | 4,064 | 4.73 |
| 10,588 | 50,001 | 186.7 | 3,724 | 4.72 |

Two corrections to earlier readings of this same run, both recorded because the pattern matters more than either number.

**The database projection was wrong by roughly 14x.** An early sample (3,043 items, 152 MB) implied ~52 KB per item and a 21 GB database at TV scale. That was SQLite preallocation: the file jumped to about 161 MB within the first 5,000 items and then stayed nearly flat while the item count grew tenfold. The marginal cost is ~3.7 KB per item.

**The rate was not degrading.** An earlier reading suggested it was. Measured to completion the rate *improved* through the run, from 3.72 to 4.75 items/second, and settled at 4.72.

Both mistakes came from extrapolating an early window of a system with warm-up and preallocation behavior, and both erred toward alarm. Projections in this area should come from completed runs.

### Projections

| Catalog | Scan time | Projected DB |
|---|---|---|
| 50,000 movies | 2.9 hours (measured) | 187 MB (measured) |
| 400,000 TV episodes (20 peers) | ~23.5 hours | ~1.5 GB |

The surviving constraint is **time, not disk**. A first join to a 20-node group with substantial TV libraries is close to a full day of continuous scanning, during which section 5 finding 6 applies: the operator has no usable progress signal. Disk growth is unremarkable and should be dropped as a concern.

## 5. A3: music federation pollutes the library, and metadata does not fix it

This was the remaining feasibility gate from `design-spec.md` section 9. It fails.

A three-track album was placed twice in one Music library: once as real MP3 files with embedded ID3 tags (the local copy), and once as `.strm` references to the same tracks through the peer origin.

### First pass, filename and tag driven

| | Result |
|---|---|
| Artists | **2**, both named `Test Artist` |
| Albums | **2**: `Test Album` (local, from ID3) and `Test Album (2015)` (remote, from folder name) |
| Tracks | **6** (full duplication) |
| Remote track titles | Raw filenames, because `.strm` carries no embedded tags |

### Second pass, with NFO metadata and conventional filenames

`artist.nfo` and `album.nfo` were added to both roots, each declaring the same MusicBrainz artist and album identifiers, and the remote `.strm` files were renamed to the conventional `NN - Title` form.

| | Result |
|---|---|
| Artists | **2**, unchanged |
| Albums | **2**, unchanged |
| Tracks | **6**, unchanged |
| Local album provider IDs | `MusicBrainzAlbum`, `MusicBrainzReleaseGroup`, values obtained from an online lookup rather than from the NFO |
| Remote album provider IDs | **`{}`** — none |

The declared MusicBrainz identifiers in `album.nfo` were not honored for either root. The local album acquired different identifiers from an online lookup; the remote album acquired none at all.

### Why this is worse than the video case

For movies, `MergeVersions` exists as a mitigation (section 3) and folder plus NFO naming produces a usable item. Music has no equivalent:

- Jellyfin has no alternate-version concept for audio tracks, so there is nothing to merge into.
- `.strm` files carry no embedded tags, and tags are what Jellyfin's music model builds artists and albums from.
- The damage is not confined to the album. **The artist list itself duplicates**, so a user browsing by artist sees every federated artist twice, once per contributing source. At twenty peers that is a browse surface with up to twenty duplicate entries per shared artist.

### Consequence

The escape hatch in section 9, "record the exception rather than creating misleading duplicate cards," is no longer sufficient, because the duplication is not optional. Three real options:

1. **Cut music from v1.** Cleanest, and consistent with the confirmed MVP decision to defer music polish.
2. **Namespace remote music into a separate `Friends Music` library.** Accepts duplication but contains it, so the local artist list stays clean. This contradicts the integrated-by-collection-type decision for music specifically, but music is the collection type that gains least from integration, since there are no cross-source versions to group.
3. **Elect a winning source per album.** Directly violates the confirmed rule that Jellymesh never automatically prefers one source's version.

Option 1 for v1, with option 2 as the shape if music returns later.

## 6. Remaining Phase 0 coverage

### Subtitles: work as sidecar files, and must therefore be copied

An external `.en.srt` placed beside a remote `.strm` attached correctly:

```text
codec=subrip  language=eng  IsExternal=True
```

This confirms review finding C4. Subtitles cannot be relayed lazily, because Jellyfin needs the file present on disk to enumerate the track. Subtitle **content** must therefore be copied to the destination, which is a real and currently unstated exception to "Jellymesh does not copy media files." The generated layout in `design-spec.md` section 10 has no place for subtitle files and needs one.

### Source deletion: clean

Removing the source `.strm` and rescanning removed the item from the catalog, and the stream endpoint then returned 404 for the previously valid item ID. No playable orphan remained. The tombstone path behaves as the design assumes.

### Per-user progress isolation: confirmed

Two local users set different resume positions on the same remote item and both persisted independently:

```text
admin      position = 300000000 ticks (30 s)
housemate  position = 900000000 ticks (90 s)
```

This validates the confirmed decision that watch state, resume position, and play count remain local and per-user.

One methodology note. An initial attempt using `POST /Users/{uid}/PlayingItems/{id}/Progress` returned 204 but stored position 0 with `Played=true`, which looked like a `.strm` defect. A control against a local file in the same library behaved identically, showing the cause was the test method rather than remote media: that endpoint does not persist a resume point without an active play session. The isolation result above uses `POST /Users/{uid}/Items/{id}/UserData`. The control is recorded because the first reading would otherwise have been filed as a federation bug.

## 7. Findings not anticipated by the review

1. **The peer URL is returned to clients.** `PlaybackInfo` returns `Path` as the raw origin URL (`http://peer-origin:8099/...`). Any credential embedded in a `.strm` URL would be handed to every client that opens the item. This is empirical confirmation of review finding B6 and raises its priority.
2. **Jellyfin proxies rather than redirects.** A client range request to `/Videos/{id}/stream` produced no redirect; Jellyfin fetched from the origin and relayed. The relay-backed playback model is sound.
3. **Range pass-through is byte-exact.** A client request for `bytes=0-5242879` produced exactly one origin request for `bytes=0-5242879` and exactly 5,242,880 bytes. No amplification, no full-file buffering.
4. **The `[tmdbid-NNNNN]` folder convention triggers external metadata resolution, and the failure mode is severe.** Two synthetic identifiers were tested and both resolved to unrelated real TMDB entries: `999001` renamed `Sentinel Probe` to `Daal Baati Churma`, and `999002` renamed `Doomed Title` to `Asian POV 4`, an adult title. Both occurred with the library reporting `EnableInternetProviders = False`.

   The specification's generated layout in section 10 uses exactly this convention. Two consequences follow. A stale, mistyped, or colliding identifier in a peer's catalog does not merely mislabel an item, it can pull an unrelated title and its artwork into a destination library, including a library a household considers family-safe. And the destination appears to re-resolve against TMDB rather than trusting the generated NFO, which is both a correctness problem and an outbound disclosure of what a peer's catalog contains.

   This should be treated as a design defect in the generated layout, not a cosmetic issue. Candidate mitigations: keep provider identifiers in NFO only and out of directory names, disable internet providers on generated roots and verify that setting is actually honored, or namespace generated folders by a Jellymesh-internal identifier that no metadata provider will match.
5. **`MediaSource.Size` is never corrected.** It stays at the `.strm` file's own byte count (75) even after a successful probe populates runtime and streams.
6. **Scan progress reporting is unusable at this scale.** Across a 20-minute observation window the `Scan Media Library` task reported 80.16% to 81.44% while the indexed item count rose steadily from 660 to 4,563. An operator watching the dashboard has no way to estimate completion, and a multi-hour first join will appear hung. Any Jellymesh onboarding flow that waits on a library scan needs its own progress signal rather than relying on Jellyfin's.

## 8. Privacy and authorization surface

These tests were not in the original review. They were run because section 2 showed Jellyfin exposing more through `PlaybackInfo` than the design assumed.

A second local user (`housemate`) was created on the consumer: not an administrator, and then restricted to `EnableAllFolders: false` with `EnabledFolders: []`, meaning no library access whatsoever.

| Test | Endpoint | Result |
|---|---|---|
| Enumerate items | `/Items?Recursive=true` | **Denied.** 0 items returned. |
| Fetch item by ID | `/Items/{id}` | **Denied.** 404. |
| Fetch item by ID, user scope | `/Users/{uid}/Items/{id}` | **Denied.** No item. |
| Playback decision by ID | `/Items/{id}/PlaybackInfo` | **Denied.** 0 media sources. |
| **Stream by ID** | `/Videos/{id}/stream` | **ALLOWED. 206, content served.** |

### The stream endpoint is not gated by library permissions

A user with zero library access streamed 2,097,152 bytes of the peer's media. The byte-accounting origin recorded exactly 2,097,152 bytes served for that request, proving the content was fetched from the peer and relayed rather than served from a cache, and `ffprobe` confirmed real h264 1280x720 video with AAC audio.

Jellyfin's library access control gates **metadata**, not the raw stream route.

This contradicts a confirmed product decision. Follow-up decision 4 states: "reuse normal Jellyfin library permissions. Source publication and destination opt-out control server-to-server access; Jellyfin controls local user access." That holds for browsing. It does not hold for playback.

Three consequences for the design:

1. The claim that local Jellyfin permissions decide which local users may access remote media must be narrowed to **browse** access. It is not an access-control boundary for bytes.
2. The relay cannot assume that Jellyfin authorized a request before forwarding it. Because the relay is invoked by Jellyfin rather than by the end user, it also cannot see which local user triggered playback, so per-user policy cannot be enforced at the relay either. If per-user enforcement is required, it needs a mechanism that does not exist in the current design.
3. The protected-library guarantee is not weakened by this, because a protected library is never materialized and therefore has no item ID to request. The exposure is to **published** remote media whose item IDs are known.

### Severity

The attack requires knowing a 32-hex item ID. Those IDs were tested against several path-hash derivations (`md5` of the path in original, lowercase, and uppercase forms, each in UTF-8 and UTF-16LE) and none matched, so IDs appear not to be computable from a path on 10.11.11 and are not brute-forceable at 128 bits.

The realistic exposure is therefore a leaked rather than a guessed identifier: a user whose access was revoked but who retains previously seen IDs, a client cache, a shared link, logs, or a household member with partial library access. That is a plausible household scenario rather than a remote attack, and it should be recorded as a known limitation rather than a blocker.

### Peer URL disclosure to ordinary users

Before the restriction was applied, the same non-administrator user could read the full peer origin URL through two separate routes:

```text
/Items?Fields=MediaSources   -> MediaSource.Path = http://peer-origin:8099/...
/Items/{id}/PlaybackInfo     -> MediaSource.Path = http://peer-origin:8099/...
```

Local filesystem paths for local media were disclosed the same way. This is empirical confirmation of review finding B6 and raises its severity: any household member on a consumer server can extract a peer's endpoint address. In production that endpoint is publicly reachable over HTTPS, so the disclosure is actionable rather than theoretical.

It follows that a `.strm` must contain a **local** relay URL, that the relay URL must not itself be a bearer credential, and that the relay must authenticate the request rather than trusting the URL. The design should state all three explicitly.

## 9. Code fixes applied

All build clean, `go vet` clean, all packages pass.

| Review finding | Change |
|---|---|
| D5 | `MergePreservingLocal` no longer resurrects a deliberate un-watch. When the local record is the newer of the two it is authoritative in full. Two regression tests added. |
| D7 | Added `BackoffWithJitter` with a caller-supplied random source, plus a documented `DefaultJitterFraction`. `Backoff` is retained and documented as the un-jittered base. Three tests added. |
| D9 | `policy.State` and `group.State` both take an injectable `Now func() time.Time`. Internal `time.Now()` calls now route through it. This unblocks Phase 0 steps 19 and 20, which otherwise require waiting fifteen real days. |
| D11 | `group.Eject` returns a new `ErrNotAdministrator` instead of the misleading `ErrNotMember`. `MarkOwnerAvailable` no longer strands the state machine in a transitional state past the deadline. |

| D1-D4 | Membership model reconciled onto owner/administrator. `internal/policy` now consults `internal/group` for roles, the member roster, succession, and dissolution instead of keeping a divergent copy; policy's duplicate dissolution state machine is gone. Zero `Coordinator` references remain. Administrators can now approve invitations (**D2**), which the coordinator gate had prevented despite the specification requiring it. A **candidate publication** set was added so a joining or rejoining node can satisfy the non-empty-publication admission rule before it is a member (**D3**); `Publish` is now gated on membership, which closed a separate hole where any node, including an ejected one, could inject into the live pool. 24 tests. |
| B1 | Succession no longer applies on a local timer. `Advance` reports eligibility; `ClaimOwnership` requires absence attestations from a quorum of distinct other members and rejects self-attestation, owner attestation, non-members, duplicates, and observations too recent to support the claim. 8 tests, 3 adversarial. |
| D12 | `internal/config` rewritten for the mTLS decision: three separate listeners (public federation, loopback relay, loopback admin), node key and certificate paths, public hostname, Jellyfin base URL and API key, generated root. Listener collisions refused; federation without a hostname or Jellyfin key fails at load. 11 tests. |

Not attempted: **D10**, the persistence layer. See section 10.

## 10. D10 persistence: recommendation, not a decision taken

Every store in `internal/` is still an in-memory map that dies with the process. This is the largest remaining structural gap and it was deliberately left undone, because resolving it means adding this project's **first external dependency** and `go.mod` currently has no requirements at all. That is a standing choice worth making explicitly rather than absorbing into a refactor.

### What the data actually demands

Tonight's measurements set the requirements:

- **Catalog mappings.** Section 4 measured 50,001 items indexed from one synthetic source. At twenty peers with comparable libraries the mapping table is on the order of a million rows, keyed by `(source_node_id, source_item_id)` and scanned by source node during ejection and resynchronization.
- **Retention expiry.** `RetentionStore.Expire` currently walks every record on every call. With an indexed `expires_at` this is a range scan; without one it stays O(n) over the whole catalog.
- **History ledger.** Keyed by `(local_user_id, logical_work_id)`, read on item reappearance.
- **Small state.** Peers, trust, publications, opt-outs, blocks, invitations, sync cursors. Trivial at any scale.

Two access patterns therefore want real indexes: catalog-by-source-node and retention-by-expiry.

### Options

**SQLite via `modernc.org/sqlite` (recommended).** Pure Go, so cross-compilation for the container image stays trivial and there is no cgo toolchain in the build. Gives indexes, transactions, and range queries directly. An operator can inspect state with standard tools during a failure, which matters for a product whose release gates include backup, restore, and rollback drills. Backup is a file copy plus a WAL checkpoint, a well-trodden path.

**bbolt.** Also pure Go, simpler, ACID, no SQL. The cost is that every index becomes a hand-maintained bucket, including the retention-by-expiry index. That is exactly the kind of code that silently drifts out of sync with its primary data.

**Stay in memory with a snapshot file.** Viable only while catalogs are small. At the measured scale it is not, and it would have to be replaced before the first real multi-peer deployment.

### Recommendation

`modernc.org/sqlite`, introduced behind a storage interface so the backend stays replaceable. The indexed access patterns are real rather than speculative, and the operational story during an incident is much better than a binary KV file.

The counter-argument deserves a hearing: a dependency-free service is genuinely easier to audit and to keep building for years, and bbolt is a smaller surface than SQLite. If the preference is to stay closer to zero dependencies, bbolt is defensible, and the cost is paid in hand-written index maintenance rather than in build complexity.

This needs ratification before Phase 1 goes far, because the storage boundary shapes every package that touches state.

## 11. Blocked

- **Cedar calibration.** An attempt to read library item counts over SSH was refused by the local sandbox as a containment escape. It was not retried or worked around. The A5 projections above therefore use synthetic tiers rather than real library sizes. Real counts from Cedar would sharpen them.
- **Host-to-container networking.** The host firewall (554 nftables rules) blocks container-to-host traffic, and `host.docker.internal` resolves to the default bridge gateway rather than the lab network's. The origin service was moved into the lab network instead, which needed no host changes and is closer to the real topology.

## 12. Recommended revisions to the plan review

1. **Rewrite A1.** The extraction-inheritance risk is not real on 10.11.11. Replace it with A1-a (per-`PlaybackInfo` re-probe) and A1-b (no media metadata before first play). Neither is architecture-invalidating, so A1 should drop out of section 2 and into the security and product sections.
2. **Re-rank the sequence.** With A1 gone, the integrated-library decision rests on A2 alone, and A2 has a validated mitigation. The decision is now "are we willing to own a merge graph," which is a scoping question, not a blocker.
3. **Promote A5.** It is the strongest surviving architectural constraint and the only one whose measured numbers came in worse than estimated.
4. **Promote B6.** The peer URL reaching clients through `PlaybackInfo` is confirmed rather than theoretical.
5. **Add the metadata-resolution finding** from section 5.4 as a new item. The generated-layout naming convention may be actively harmful.

## 13. Lab state

Left running for inspection:

```bash
cd lab
docker compose -f docker-compose.yml -f docker-compose.media.yml ps
```

- peer-a: <http://127.0.0.1:18096>
- peer-b: <http://127.0.0.1:18097>

The lab instances are created through Jellyfin's startup API with credentials chosen by whoever runs the lab. Do not reuse production credentials, and do not record lab credentials in this repository.
- Synthetic media under `lab/media/`, results under `lab/results/`, both gitignored.
- `lab/docker-compose.yml` is unmodified; additions are in `lab/docker-compose.media.yml`.

To stop without losing state:

```bash
docker compose -f docker-compose.yml -f docker-compose.media.yml down
```
