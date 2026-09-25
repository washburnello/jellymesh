# Phase 0: Local Federation Lab

Status: Scaffold only. Do not use real credentials, production databases, or family media.

## Purpose

Validate whether a backend-only Jellymesh Service can make remote movies, television, and music appear in normal Jellyfin libraries while preserving:

- exact source-library publication and destination opt-out;
- protected `Family Movies` denial;
- native multiple-version behavior;
- friendly source-server labels;
- public HTTPS reachability;
- local-only progress;
- source revocation;
- safe behavior during provider outages.

## Lab topology

The initial lab uses two disposable Jellyfin instances on Walnut:

```text
Walnut peer A: 127.0.0.1:18096
Walnut peer B: 127.0.0.1:18097
```

The compose file is [docker-compose.yml](docker-compose.yml). It creates persistent lab state under `lab/data/` and does not publish a public port or touch Cedar.

The first test runs locally for quick catalog and playback experiments. The later federation test must use separate HTTPS hostnames controlled by the tester and must not rely on the local-only ports for public reachability.

## Synthetic libraries

Each test node should eventually contain:

```text
Shared Movies       published to group
Shared TV           published to group
Shared Music        published to group
Family Movies       private
Personal            private
```

Use harmless generated media or test clips. Do not copy Cedar’s production database or media into the lab.

## Phase 0 sequence

1. Start the two local Jellyfin instances.
2. Complete first-run setup separately for each instance.
3. Add synthetic libraries and local users.
4. Confirm each server works independently.
5. Request group membership with no published library and confirm the request is rejected.
6. Stage or implement a disabled-by-default Jellymesh Service.
7. Generate node identities and key material.
8. Establish one-way and then two-way peer trust in a test environment.
9. Publish `Shared Movies` into the group pool.
10. Confirm `Shared Movies` auto-appears at the destination without a separate acceptance action.
11. Keep `Shared TV` published but explicitly opt out of it at the destination.
12. Confirm the opted-out library is absent from catalog, search, artwork, and stream access.
13. Add a new movie to `Shared Movies` and confirm it appears automatically at the destination.
14. Confirm `Family Movies` cannot be selected or enumerated.
15. Add the generated remote Movies root to the existing Movies library; do the equivalent for TV and Music.
16. Add a second source version for one movie and one TV episode.
17. Confirm native Jellyfin grouping, destination-local collection creation, and source labels.
18. Test that no source collection or playlist is created or modified by the consumer.
19. Simulate owner loss and confirm the group waits 15 days before selecting a new owner from the admin pool.
20. Simulate no owner/admin availability and confirm timeout notifications precede group dissolution.
21. Unblock the peer and confirm prior opt-outs are preserved.
22. Eject one ordinary member through an owner or admin, stop the rejected server from cooperating, and confirm every remaining member independently denies access and purges generated content.
23. Attempt re-entry with a new invitation and fresh approval, then confirm normal pool behavior returns only after the new admission is accepted.
24. Test direct play, seeking, subtitles, audio tracks, and HLS/transcoding.
25. Test playback progress with two local users on the consumer.
26. Stop the source Jellymesh Service or source Jellyfin and test safe playback failure.
27. Restore the source and test recovery.
28. Remove the source library from the group pool and test new catalog and playback behavior.
29. Revoke the entire peer relationship and test unrelated future operations.
30. Add a third disposable node only after the two-node path passes.
31. Run the same test from an external network using public HTTPS.

## Required test data

The lab should include:

- one movie present only on peer A;
- one movie present on both peers with different quality labels;
- one movie with no strong provider ID;
- one TV series with one episode on peer A and another episode on peer B;
- one TV episode present on both peers;
- one music album with tracks and artwork;
- one ambiguous duplicate title/year pair that must not be merged;
- subtitles in at least one text format;
- one item deleted from the source during a later run.

## Acceptance matrix

| Area | Test | Pass condition |
|---|---|---|
| Invitation | Existing member creates invite; owner/admin receives pending request | Owner/admin can approve or deny; approved member is admitted after publication |
| Owner succession | Owner unavailable for 15 days | Oldest administrator becomes owner; exact timestamp ties use member ID |
| Admin limits | Admin attempts to eject another admin or owner | Operation is rejected; owner-only controls remain protected |
| Owner/admin outage | No owner/admin available | Immediate notice, notifications every 5 days, and dissolution after 15 days |
| Ejection | Eject one ordinary member through owner/admin and stop its cooperation | Every remaining member denies access and purges generated content; unrelated peers remain unaffected |
| Rejoin | Rejoin with a new invitation and fresh approval | Old access is not restored; new admission works only after new approval |
| Admission | Request group membership with no published library | Membership is rejected |
| Group pool | Publish Shared Movies | It auto-appears at every member unless opted out |
| Group pool | Add a new movie to Shared Movies | New content appears automatically at non-opted-out destinations |
| Block | Block one peer | Media exchange stops both ways; both nodes remain members |
| Unblock | Unblock and inspect decisions | Prior opt-outs remain in effect |
| Library policy | Opt out of Shared TV at one destination | The opted-out library is absent from catalog, search, artwork, and stream access there |
| Privacy | Attempt Family Movies publication | Publication is rejected or the library is never selectable |
| Privacy | Guess item ID or stream path | Protected item cannot be read or played |
| Catalog | Restart source Jellyfin | Known-good remote catalog remains intact |
| Catalog | Delete an item at the source | Item disappears from the visible Jellyfin library immediately; retention metadata/artwork/history remain for 7 days by default, then purge if it does not return |
| Sync | Run a successful hourly incremental sync | Only changes after the cursor are applied; no full catalog download |
| Sync | Interrupt a catalog sync | No deletions occur; the previous cursor remains usable |
| Sync | Simulate source restart and clock drift | Recovery does not create phantom deletions or excessive requests |
| Catalog | Source remains offline | Catalog remains visible for 15 days; playback is denied and cleanup happens after the retention window |
| Grouping | Same movie on two peers | One logical card and multiple native versions, if supported |
| Collections | Franchise split across local and remote movies | Destination can create and manage the collection locally |
| Collections | Consumer syncs a source collection | No source collection is created or modified |
| Playlists | Playlist contains local and remote songs | Playlist remains local to the destination and is not broadcast |
| TV | Same episode on two peers | One episode with multiple versions, if supported |
| Music | Album and tracks | Native grouping and playback meet the approved behavior |
| Playback | Direct play | Media streams and seeks correctly |
| Playback | HLS/transcode | Jellyfin can transcode the remote source if required |
| Transcoding | Start multiple remote transcodes at one destination | One runs by default; additional sessions are limited or configurable |
| Subtitles | Text subtitles | Tracks or burned subtitles behave as documented |
| Client matrix | Web, Android, Android TV, iOS/Swiftfin, and Roku TV | Each target client can browse, play, seek, and report local progress |
| Health visibility | Compare peer health view with local admin diagnostics | Members see aggregate status; detailed failures and sanitized diagnostics are local-admin-only |
| Settings | Configure published libraries, opt-outs, transcode limit, invites/roles, and sync health | Local settings work; group defaults apply to nodes without overrides, local overrides win, and changes are auditable |
| Identity | Same title/year from two sources | Merges only with a strong provider identity; ambiguous matches stay separate |
| Version selection | Multiple source versions on one logical work | No automatic source preference; native version selection remains available |
| Progress | Two local users | Progress is isolated per local user and never sent upstream |
| History | Dissolve group, then restore the same work from another source or a local copy | Missing watched state, play count, and resume position are restored; newer local progress is not overwritten |
| Revocation | Remove a source library from the group pool | New remote access fails and source items are purged or marked according to policy |
| Revocation | Remove peer | New peer requests fail; unrelated peers remain unaffected |
| HTTPS | External network test | Clients and peers connect without Tailscale |
| Operations | Kill process during sync | Previous valid generated state remains usable |

## Safety gates

- Keep the raw Jellyfin administration endpoint private.
- Use synthetic media only.
- Do not install a federation plugin on Cedar during the first local test.
- Do not publish Family Movies even in a test unless the test is specifically verifying denial.
- Do not copy production API keys, user tokens, cookies, or databases into the lab.
- Do not commit generated media, keys, tokens, logs, or database files.
- Treat public HTTPS hostnames and certificates as deployment assets, not test data.

## Phase 0 exit decision

Proceed to implementation only if the local lab answers these questions clearly:

1. Can stock Jellyfin clients display and play the remote content?
2. Do movies and TV episodes group as desired?
3. Is music behavior acceptable?
4. Can the source name be shown without custom clients?
5. Does the relay path meet the expected playback behavior?
6. Can exact per-library privacy be enforced at every tested route?
7. Can a normal home server deploy and recover the Jellymesh Service safely?

If any answer is no, record the failing behavior in the design specification before choosing between a Jellyfin upstream contribution, a custom presentation adapter, or a different playback path.
