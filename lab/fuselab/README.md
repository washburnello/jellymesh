# FUSE gate lab (#73)

The FUSE spike's gates (#60, `lab/fusespike/`), rerun against the real
build. What the spike's stand-ins did is done here by the product:

```text
source Jellyfin -> fault proxy -> source node (fl-src)
    -> destination node's read service (fl-dst, JELLYMESH_PRESENTATION=fuse)
    -> jellymesh mount (fl-mount) -> destination Jellyfin (fl-dst-jf, uid 7777)
```

- `env.sh up` builds the image, hard-links the three Back to the Future
  films and 30 synthetic copies (other titles and identities) into the
  source's library, starts everything, forms a group (the destination
  founds it, the source joins and publishes), materializes, and adds the
  mount's films to the destination Jellyfin as "Jellymesh Movies" with
  extraction off.
- `env.sh down` removes every container, volume, and mount.
- `gates.py g1|g2|g3|g3_extraction|g6|g7` runs a gate.
- `faultproxy/` sits between the source node and its Jellyfin. It counts
  media bytes and injects the spike's faults: refuse, hang, crawl, and
  latency.

Everything runs on walnut, in its own containers and
`~/.local/share/jellymesh-fuselab`. The destination Jellyfin is on
`127.0.0.1:18230` and the LAN (`192.168.87.20:18230`), for G4 on real
devices. Its credentials are in `credentials` there and are never printed.

Differences from the spike:
- No `.strm` library side by side: a node has one presentation. The G6
  `.strm` figures are the spike's.
- The "reader" is the destination node itself, so "reader stopped" stops
  the node.
- The source's upload ceiling is off (`JELLYMESH_UPLOAD_CEILING_MBPS=0`),
  so G6 measures the chain, not the ceiling.
- Past each film's burst, reads as fast as possible are paced (#69). G6
  reports throughput inside the burst and the paced rate past it.

## Results (2026-10-01, walnut, Jellyfin 10.11.11, kernel 7.2)

**First scan.** All 33 films were probed through the mount, with runtime,
codecs (HEVC, AAC, ASS), and the exact size (1469241147 bytes for the first
film). Each film cost the source one 1 MiB chunk (34.6 MB in all), as in
the spike.

**G6, feels normal: PASS.** A fresh node each round, so no cache helps.

| | 0 ms | 40 ms per source request | spike, 40 ms |
|---|---|---|---|
| PlaybackInfo | 0.03 s | 0.02 s | |
| Start (first byte / 1 MB) | 0.05 / 0.08 s | 0.13 / 0.21 s | 0.13 s |
| Seek to 700 MB (first byte / 1 MB) | 0.04 / 0.07 s | 0.09 / 0.18 s | 0.10 s |
| Sustained, inside the burst | 140 MB/s | 117 MB/s | 322 MB/s |
| Read as fast as possible past the burst (paced) | 2.2 MB/s | 2.2 MB/s | |

Sustained is lower than the spike's because two real nodes and mutual TLS
are now in the path; 117 MB/s is still about 470 times this film's bitrate.

**G1, never hangs, and G5, recovers by itself: PASS.** A stream at 200 MB
into a film, then each fault:

| Fault | Stream ended | Jellyfin ping worst / failures | Threads in D | Next fresh read works |
|---|---|---|---|---|
| source refuses | 5.6 s | 0.01 s / 0 | 0 | 0.2 s |
| source hangs | 30.1 s | 0.00 s / 0 | 0 | 0.2 s |
| source crawls (1 kB/s) | 30.1 s | 0.00 s / 0 | 0 | 0.2 s |
| destination node stopped | 2.1 s | 0.00 s / 0 | 0 | 0.2 s |
| mount crashed | 2.3 s | 0.01 s / 0 | 0 | 0.2 s |
| mount deadlocked | 20.8 s | 0.00 s / 0 | 0 | 0.2 s |
| mount frozen (cgroup) | 22.2 s | 0.00 s / 0 | 0 | 0.1 s |

The first run of the three mount faults found a harness error, not a
product one: `docker kill --signal` marks a container as stopped by hand, so
its restart policy never acted. The harness now signals from a helper
sharing the mount's PID namespace, as a crash from inside would; Docker
then restarted the mount each time.

**G3, no surprise transfers: PASS, with the requirement unchanged.**
- A scan with nothing new: 5.2 MB. These were the re-probes of five films
  whose reads had failed under G1's faults, healed and marked changed, as
  designed.
- Every scheduled task (17): 36.2 MB, the Keyframe Extractor reading each
  film's index once, as in the spike.
- A full metadata refresh: 0 MB.
- Films are unreadable to everyone but Jellyfin's uid: `cat`, `md5sum`, and
  `grep -r` by walnut's user, and a read from another container as uid 1234,
  were refused, and 0 bytes crossed.
- With trickplay extraction left on (G3b), the task read one film at about
  87 MB a minute (the spike saw 104), bounded by its own decoding speed.
  That is only about 5.6 times this 2 Mbit/s film's bitrate, so pacing
  (four times the bitrate, at least 1 MiB/s) would trim it by about a third,
  once the 150 MB burst ran out after about seven minutes. Pacing bounds a
  fast extraction; it does not make a slow one cheap. Extraction off stays
  a hard requirement.

**G2, the library never shrinks: PASS.** No item or watched state was lost
in any of the five scenarios. Two gaps the spike's write-up had not checked
were found and fixed:
- E: films first scanned while the destination node was down got their
  media information only once the mount kept and resent the failures the
  node could not take (0 of 6, then 6 of 6 after the node returned).
- C: two of twelve films whose probes a mount crash cut off never healed,
  because the failure never reached the node. The node now retries every
  film read in the two minutes before a mount restart. Rerun: 11 of 12,
  then 12 of 12 after healing and a rescan.

**G7, soak: PASS** (2026-10-01 01:23 to 07:30, 331 cycles, 0 with
problems). Two films streamed continuously, with one random disruption per
cycle: mount crashed 62 times, frozen 46, deadlocked 36; source refused 36,
hung 34, crawled 25; destination node stopped 33; Jellyfin restarted 11;
films added 19 times (3 each; 54 to 111 items); scans 29. After every cycle
a fresh read worked (worst 12.1 s), no Jellyfin thread stayed in D, the
ping stayed fast, and no item or watched state was lost. The mount
restarted 145 times by itself. Memory at the end: mount 12 MiB, node
539 MiB (a 256 MiB chunk cache under GOMEMLIMIT 600 MiB), Jellyfin 296 MiB.

Two earlier starts of the soak were stopped for harness flaws, not product
ones. At 00:45, cycle 18 counted two Jellyfin threads in D at two moments
25 s apart, while builds were running on the same disk. A later count found
none, and the check did not compare thread identities. At 01:14, the
restarted run reused the earlier run's titles for added films. The check
now follows thread IDs, and added films get unique titles.

**G4, real devices:** needs the user's Roku and phones, against
`http://192.168.87.20:18230`.
