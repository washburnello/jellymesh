# Jellymesh pilot: setting up a second household

**Status: draft (#58).** Two items are marked **TO DECIDE** and must be
filled in before this is sent: how the software is obtained, and the address
to join.

This packet is written for the person setting up, and for an assistant (such
as an AI coding agent) working on their behalf. Follow it in order. Every
step says how to check it worked. Never paste passwords, tokens, API keys, or
backup passphrases into messages back to us; nothing below needs them.

## 1. What this is

Jellymesh links independent Jellyfin servers owned by friends and family.
Each household keeps its own Jellyfin, users, and watch history. It chooses
which libraries to share, and sees the others' shared libraries alongside its
own. Nothing is copied: films stream from the household that has them when
someone plays them.

In this pilot you join a small group run by the Washburn household and share
at least one library of yours (joining requires offering one; it can be a
small one). You can stop at any time; see section 10.

What others in the group can see:
- the titles and metadata of the libraries you choose to publish, and
  nothing else;
- your node's advertised address;
- when they play one of your films, the file's bytes, through Jellymesh.

## 2. Requirements

- **A Linux host with Docker** and Docker Compose, the machine that runs
  Jellyfin or one beside it.
- **Jellyfin 10.11.x.** Only 10.11.11 has been tested.
- **A reachable address**, one of:
  - a router port forward of **TCP 8443** to this host; or
  - a free **Tailscale** account and Funnel in raw-TCP mode, if you cannot
    forward ports.

  `docs/operator-reachability.md` covers both, and how to check from
  outside.
- **UDP 44843** forwarded too, if you can. It is optional but makes playback
  go directly between our homes.

**Check:** `docker compose version` and your Jellyfin's dashboard shows
10.11.

## 3. Get the software

**TO DECIDE:** GitHub access to the repository, or an image file we send.

- *If the repository:* `git clone <repository URL>` and build with
  `docker compose -f deploy/docker-compose.yml build`.
- *If an image file:* `docker load < jellymesh-<version>.tar`, then use
  `deploy/docker-compose.yml` from this packet with `image:` set to the
  loaded tag.

**Check:** `docker image ls | grep jellymesh` lists the image.

## 4. Prepare Jellyfin

1. **A service user for Jellymesh.** In Jellyfin's dashboard, create a user
   named `jellymesh`:
   - with a strong password;
   - **not** an administrator;
   - with access only to the libraries you are willing to share.

   Jellymesh reads your server as this user and can never see more than it
   can.
2. **Libraries that must never be shared.** Note the library ID of any
   library that must never be shared (a family videos library, for
   example). Jellymesh refuses to publish these even if the user can see
   them.
3. **A folder for other households' films.** Create one, for example
   `/srv/jellymesh-generated`:
   - owned by uid 65532, mode 0755, so Jellymesh can write it;
   - mounted into your Jellyfin container too, so Jellyfin can read it.

**Check:** you can log in to Jellyfin as `jellymesh` and see exactly the
libraries you intend.

## 5. Configure and start the node

Copy `deploy/docker-compose.yml` and set, at least:

| Setting | Value |
|---|---|
| `JELLYMESH_PUBLIC_HOSTNAME` | your reachable address with port, for example `yourhome.example.org:8443` or `<machine>.<tailnet>.ts.net:10000` |
| `JELLYMESH_NODE_NAME` | a short household name; others see it as the version label of your films |
| `JELLYMESH_JELLYFIN_URL` | how the container reaches your Jellyfin |
| `JELLYMESH_JELLYFIN_USER`, `JELLYMESH_JELLYFIN_PASSWORD` | the service user (put the password in `.env`, not the file) |
| `JELLYMESH_PROTECTED_LIBRARIES` | the IDs from step 4.2, comma-separated |
| `JELLYMESH_GENERATED_ROOT` | the folder from step 4.3, as mounted in the container |

Keep the relay published on `127.0.0.1` only, as in the file. Then:

```sh
docker compose up -d
docker compose exec jellymesh /jellymesh status
```

**Check:** `status` answers, and its `direct` section shows `enabled: true`
with an `outside_address`. If it shows `off_reason`, see the operator guide.

## 6. Check you are reachable

From a phone **with Wi-Fi off**, open `https://<your advertised address>`.
A certificate warning or error means reachable. A page that spins until it
times out means blocked. Joining checks this too, and is refused otherwise.

## 7. Publish at least one library

```sh
docker compose exec jellymesh /jellymesh libraries
docker compose exec jellymesh /jellymesh publish <library-id> -root <the library's folder as Jellyfin sees it>
```

**Check:** `publish` succeeds. It refuses a protected library, and a library
with files outside the roots you declare.

## 8. Join the group

We send you, over a channel we both trust (a phone call or a message app):
- **an address to join:** **TO DECIDE**, which depends on how the Washburn
  node is reached (#48);
- **a one-time short code.**

```sh
docker compose exec jellymesh /jellymesh join -address <address> -code <short code> -wait 10m
```

While it waits, send us the **fingerprint** from your `status`. That is the
long hex string: it is public and safe to share. Read it to us by phone,
because we approve only after confirming it matches what we see. The
command finishes when you are admitted.

**Check:** `status` shows the group and its members.

## 9. See the shared films

```sh
docker compose exec jellymesh /jellymesh remote         # what the others publish
docker compose exec jellymesh /jellymesh catalog-sync   # fetch it now
docker compose exec jellymesh /jellymesh generated      # the folders to add to Jellyfin
```

In Jellyfin, add each folder `generated` lists as a **new, separate
library**, for example "Friends Movies" and "Friends TV", with these options
off:
- trickplay image extraction;
- chapter image extraction;
- saving metadata into media folders.

Then run "Scan Media Library" (or wait for its schedule; hourly is a good
setting).

**Check:** play one of the shared films. Try seeking and subtitles. After
playing, `status` → `direct.peers` shows `direct` or `tcp` for our node.

## 10. Reporting back, updates, and stopping

**What to send us:**
- the output of `status`, which holds no secrets;
- which apps you played on (Web, phone, TV) and what happened;
- anything odd, with the time it happened.

**Never send** passwords, `.env`, the admin token (`/data/admin-token`), or
backups.

**Updates:** for now, when we tell you a new version is ready, update the
repository or load the new image, then run `docker compose up -d`. A signed,
automatic update channel is planned.

**To stop:**
1. Run `/jellymesh leave`.
2. In Jellyfin, empty and rescan the "Friends" libraries. Leave an empty
   subfolder inside each, because Jellyfin skips a completely empty folder
   when removing items.
3. Remove those libraries after the scan.
4. Run `docker compose down` and delete the data volume.

Your own libraries and watch history are never touched.

## 11. If something goes wrong

`docs/operator-reachability.md` covers reachability and direct paths. For
anything else, send us the time, what you did, and the output of `status`.
