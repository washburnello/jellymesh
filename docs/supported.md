# What is supported

This records what has been tested, and how. It is updated as tests are run;
anything not listed is untested. The evidence behind each entry is in
`docs/conformance.md`.

## Jellyfin

| Version | Status | Evidence |
|---|---|---|
| 10.11.11 | Tested: source and destination, in containers and on a production server | M-7 to M-11, the FUSE spike (#60) |
| other 10.11.x | Expected to work, untested | |
| 10.10 and earlier | Unsupported | Withdrawn items' watch state is kept only from 10.11 (A-14) |

Rerun M-7, M-8, M-10, and the spike's gates on every Jellyfin upgrade: these
behaviours are Jellyfin's, not Jellymesh's.

## Clients

Playing a remote film, 2026-09-29, against Jellyfin 10.11.11 (M-1, M-11,
#60 G4):

| Client | Remote film as `.strm` (today) | Remote film through FUSE (spike) |
|---|---|---|
| Jellyfin Web | Plays | Plays |
| iPhone, iPad (Jellyfin app) | Plays, seeks, resumes | Plays, seeks, resumes |
| Roku (Jellyfin Roku 3.2.3, Hisense 50R6+) | **Fails**: the app makes the TV fetch the `.strm` URL itself | Plays, seeks, resumes, switches subtitles |
| Android, Android TV | Untested | Untested |
| Kodi, other third-party apps | Untested; apps that play a `.strm` URL themselves will fail as the Roku does | Untested |

**Known limitation.** With `.strm` presentation, any app that plays a remote
source's URL itself rather than through Jellyfin cannot play remote films,
at home or away. The Jellyfin Roku app does this (`LoadVideoContentTask.bs`,
`isHTTPStream()`). The FUSE presentation (#60) removes the limitation on
hosts that can run it.

## Hosts

| Setting | Status |
|---|---|
| Linux, Docker | Tested (walnut: Omarchy; cedar: Omarchy with Jellyfin on the host network) |
| macOS, Windows (Docker Desktop) | Untested for `.strm`. FUSE presentation cannot run there |
| NAS systems (Synology, Unraid, TrueNAS) | Untested. FUSE needs `/dev/fuse` and mount propagation in Docker |
| FUSE presentation | Needs Linux with `/dev/fuse`, a privileged mount container, shared mount propagation, and a kernel with FUSE request timeouts (tested on 7.2). It must refuse to run otherwise |

## Networks

| Situation | What happens | Evidence |
|---|---|---|
| Single NAT with a port forward | Reachable. Direct UDP paths open, and need no punching if UDP 44843 is forwarded | Design; operator guide |
| NAT that keeps one mapping per socket ("easy"), including double NAT | Direct paths open by hole punching | NAT lab M-13 (7/7); cedar's real double NAT is of this kind |
| NAT that allocates a port per destination ("hard") | No direct path. Media uses the TCP address. `status` shows `varies_by_destination` | NAT lab M-13; C-NT-6 |
| Carrier-grade NAT, or no router access | Use Tailscale Funnel in raw-TCP mode for the advertised address | Operator guide; A-16 |
| Tailscale running on the host | Supported. The direct-path default port (44843) avoids Tailscale's 41641 | Found on walnut |
| IPv6-only | Untested. STUN prefers IPv4, and offers carry IPv4 addresses | |

## Libraries

| Content | Status |
|---|---|
| Films, including several versions and several sources | Supported (C-MA-5) |
| TV series, grouped across sources, one source per episode | Supported (C-MA-6, A-13) |
| Music | Not supported in v1 (A-3) |
| A film held both locally and remotely | Shows twice, with separate watch state (A-15) |
