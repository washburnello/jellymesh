# FUSE presentation spike (#60)

The question: can remote films appear to Jellyfin as ordinary files, so that
every app plays them, without Jellyfin ever hanging or losing library items?
The spike runs on walnut only and does not touch cedar.

## Pieces

- `cmd/mount`: the FUSE process, deliberately small.
  - The tree, sizes, and NFOs come from a local manifest, so a scan never waits on a source.
  - Film bytes come from the reader, and every read has a hard deadline (10 s).
  - Film contents go only to allowed users (Jellyfin's uid 7777).
  - It asks the kernel to abort the connection if any request waits 20 s (`RequestTimeout`, which needs `patches/go-fuse-request-timeout.patch`).
  - A watchdog reads a direct-I/O health file every 5 s and exits the process if the mount stops answering. The container's restart policy then mounts afresh, and startup detaches the dead mount.
  - A film whose read failed is retried in the background. Once it is readable again, its modification time moves forward an hour, so Jellyfin probes it at the next scan.
- `cmd/reader`: stands in for the Jellymesh daemon.
  - Fetches 1 MiB chunks and holds them in a bounded LRU cache.
  - Reads ahead only once reading is sequential, doubling up to 16 chunks.
- `cmd/source`: stands in for a remote source, with byte counting and fault injection (refuse, hang, crawl, latency).
- `env.sh up <films>` / `env.sh down`: sets up the environment. `gates.py g1|g2|g3|g6`: runs the gates.

## Results, 2026-09-29 overnight, Jellyfin 10.11.11, kernel 7.2

**G1, never hangs: PASS** (after two fixes the spike found). Seven failures, each during active streaming:

| Failure | Stream ended after |
|---|---|
| Source refuses | 6 s |
| Source hangs | 31 s |
| Source crawls at 1 KB/s | 34 s |
| Reader stopped | 10 s |
| Mount crashed | 2 s |
| Mount deadlocked | 33 s |
| Mount frozen (paused) | 35 s |

In every case Jellyfin's API stayed responsive (worst ping 0.01 s) and no thread was stuck in uninterruptible sleep. Before the fixes:

- A frozen mount left a Jellyfin thread stuck in uninterruptible sleep, fixed by the kernel request timeout.
- A mount aborted by the kernel was never replaced, fixed by the watchdog.

**G2, library never shrinks: PASS.** Five scenarios:

- A scan with the mount cleanly stopped.
- A scan with the mount crashed and left down.
- A crash in the middle of a scan that was adding 12 films.
- A reboot with Jellyfin scanning before the mount existed.
- The reader down during a scan that was adding 6 films.

In all five, no item was lost and every watched state was kept. Jellyfin treats an absent or dead mount as an inaccessible root and leaves its items alone. A film whose probe failed stayed without media information until the heal rule above (G2-E). Jellyfin ignores a one-second change of modification time; an hour is honoured.

**G3, no surprise transfers: PASS, with a requirement.**

- The first scan probes each film for about 1.0 MB. NFO `lockdata` does not prevent this.
- A rescan with nothing changed, and a full metadata refresh, read 0 bytes.
- Of the 17 scheduled tasks, only the Keyframe Extractor read anything: once per new film, a bounded read of the MKV index.
- Non-Jellyfin readers were all refused and read 0 bytes: `find` and `du` work, while `cat`, `md5sum`, `grep -r`, and a container running as another uid are refused.
- **Requirement:** with trickplay and chapter extraction enabled on a library, the trickplay task pulled 104 MB in its first minute from one film and would pull every film whole. Remote films must live in libraries with extraction off, and a Jellymesh self-test should refuse to run otherwise.

**G6, feels normal: PASS.** At 40 ms of source latency, through Jellyfin's direct stream:

| | FUSE | `.strm` |
|---|---|---|
| Start (first 1 MB) | 0.13 s | 0.05 s |
| Seek to the middle | 0.10 s | 0.06 s |
| Sustained | 322 MB/s | 783 MB/s |

FUSE reports the real file size (1.47 GB against 86 B for a `.strm`) and full media information before playback.

**G4, every app plays: PENDING**, for the user's devices. The spike's Jellyfin is on the LAN at `http://192.168.87.249:18130`; credentials are in `~/.local/share/jellymesh-fusespike/credentials`. Play from "FUSE Movies", and compare with "STRM Movies".

**G7, 24-hour soak: NOT STARTED.** The overnight session stopped at 01:15 on an API error, before the soak began.
