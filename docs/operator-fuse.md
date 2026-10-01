# Presenting remote films as files (FUSE)

By default a node gives Jellyfin each remote film as a `.strm` file naming
Jellymesh's local relay. Some apps play that URL themselves rather than
through Jellyfin, and fail: the Jellyfin Roku app always does. With FUSE
presentation, remote films appear to Jellyfin as ordinary files, so every
app plays them as it plays your own (design-spec section 11, A-17).

It needs a Linux host, and a second, small, privileged container. This guide
covers checking the host, switching, and what to watch.

## What it needs

- **Linux** with `/dev/fuse`, and a kernel with FUSE request timeouts
  (Linux 6.14 or later). Not Docker Desktop on macOS or Windows, and not
  NAS systems whose Docker lacks FUSE.
- **A mount container** (`jellymesh-mount`) with `/dev/fuse` and
  `CAP_SYS_ADMIN`. It holds no key, token, or address: it reads the
  generated folder and asks the node for film bytes over a local socket.
- **Shared mount propagation** on the host folder the films appear in, so
  Jellyfin's container sees the mount, and a fresh one after a restart,
  without restarting Jellyfin. Most systemd hosts have it.
- **Jellyfin running as its own uid**, which the mount lets read films.
  Every other user and process sees names and sizes but cannot read a film,
  so backup tools, indexers, and file shares cannot pull remote films.
- **Remote films in their own libraries, with trickplay and chapter image
  extraction off.** Extraction reads every film whole, from your friends'
  uplinks. Jellymesh cannot see library options with its non-administrator
  user, so this is on you.

## 1. Check the host

Create the folder the films will appear in, and run the self-test in the
mount container:

```sh
sudo mkdir -p /srv/jellymesh-presented
cd deploy
docker compose -f docker-compose.yml -f docker-compose.fuse.yml run --rm jellymesh-mount mount -check
```

Each line says `ok` or `FAIL` and why:

| Check | Fails when | Fix |
|---|---|---|
| `fuse device` | `/dev/fuse` cannot be opened | The host has no FUSE module, or the container was not given the device |
| `mount` | A filesystem cannot be mounted | The container lacks `CAP_SYS_ADMIN`, or AppArmor or SELinux forbids it |
| `request timeouts` | The kernel is older than 6.14 | Update the kernel, or stay on `.strm` |
| `propagation` | The folder is not shared | See "Propagation" below |

If any check fails, keep `JELLYMESH_PRESENTATION=strm` (the default).
Jellymesh never switches by itself (A-18). Run the check again after each
update.

## 2. Switch

1. Set the uid Jellyfin runs as in `JELLYMESH_MOUNT_ALLOW_UIDS` in
   `docker-compose.fuse.yml`. If Jellyfin runs as your own user or as root,
   give it a dedicated one first.
2. Start both containers:

   ```sh
   docker compose -f docker-compose.yml -f docker-compose.fuse.yml up -d
   ```

3. Give Jellyfin's container the folder, read-only and with slave
   propagation, for example `/srv/jellymesh-presented:/jellymesh:ro,rslave`.
   In Compose:

   ```yaml
   volumes:
     - type: bind
       source: /srv/jellymesh-presented
       target: /jellymesh
       read_only: true
       bind:
         propagation: rslave
   ```

4. **Check Jellyfin sees it**, since Jellymesh cannot look inside Jellyfin's
   container:

   ```sh
   docker exec <jellyfin container> ls /jellymesh/films
   ```

   It lists `Movies` and `TV Shows`.
5. In Jellyfin, add `/jellymesh/films/Movies` and `/jellymesh/films/TV Shows`
   as new, separate libraries, with trickplay and chapter image extraction
   off, and scan.

Switching from `.strm` moves every remote item to a new path. The old items
leave and the new ones arrive at the next scan, and watch history
reattaches by the items' identifiers (A-14; verified by M-14). Remove the
old `.strm` libraries once they are empty.

## 3. What to watch

`jellymesh status` has a `presentation` section:

| Field | Meaning |
|---|---|
| `mode` | `fuse` or `strm`, as configured |
| `mount` | The mount's latest report: where it is, its propagation, and its read counts |
| `mount_seen` | When the mount last reported (every 30 s) |
| `reads.failed_films` | Films whose read failed and are being retried. Once one reads again it is marked changed, so Jellyfin probes it at its next scan |
| `warnings` | Anything that needs you, below |

| Warning | Meaning and fix |
|---|---|
| "the mount has not reported" | The mount container is not running, or cannot reach the socket. Check `docker compose ps` and `docker compose logs jellymesh-mount` |
| "the mount's propagation is private" (or slave) | Jellyfin may not see the mount after it restarts. Use `rshared` on the mount container's bind |
| "this kernel does not offer FUSE request timeouts" | The host's kernel changed. Switch back to `strm` |

## How it stays safe

- A library scan never waits on a friend's server. Names, sizes, and small
  files are local.
- Every film read has a 10-second deadline and then fails. Jellyfin sees a
  read error, never a hang.
- If the mount process itself freezes, the kernel aborts it within about
  35 seconds. The mount's watchdog exits, Docker restarts it, and the new
  mount takes the old one's place.
- A film whose first read failed, for example because the friend's server
  was down when Jellyfin scanned, is retried every 10 seconds and marked
  changed once it reads, so Jellyfin probes it at its next scan.
- Every read is authorized as `.strm` playback is. A friend who withdraws a
  library or blocks you, or a library you opt out of, stops reads at once.

## Propagation

Docker carries a mount from one container to another only through a host
folder with shared propagation. Check the host:

```sh
findmnt -o TARGET,PROPAGATION --target /srv/jellymesh-presented
```

If it shows `private`, make the folder its own shared mount, and make it
so at every boot:

```sh
sudo mount --bind --make-shared /srv/jellymesh-presented /srv/jellymesh-presented
```
