#!/usr/bin/env bash
# The FUSE gate lab (#73): the spike's gates, rerun against the real build.
# On walnut only; it touches nothing outside its own containers and $BASE.
#
#   env.sh up       build, make the media, start everything, form the group
#   env.sh down     remove every container, volume, and mount
#   env.sh build    rebuild the images only
#
# Containers (network fuselab):
#   fl-src-jf    the source's Jellyfin, holding the films (hard links)
#   fl-proxy     between the source node and its Jellyfin: counts media bytes
#                and injects faults; control on 127.0.0.1:18310
#   fl-src       the source node, publishing the films
#   fl-dst       the destination node, JELLYMESH_PRESENTATION=fuse
#   fl-mount     jellymesh mount: the only container with /dev/fuse
#   fl-dst-jf    the destination's Jellyfin as uid 7777, on 127.0.0.1:18230
#                and the LAN, reading the films through the mount
#
# The mount is made at $BASE/share/films under an rshared bind, and reaches
# Jellyfin through an rslave bind of $BASE/share, as deploy/ does.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
BASE=${FUSELAB_BASE:-$HOME/.local/share/jellymesh-fuselab}
FILMS=${FILMS:-$HOME/Videos/Back to the Future}
SYNTHETIC=${SYNTHETIC:-30}
JF_UID=7777
LAN_IPS=${LAN_IPS:-192.168.87.20}
GO=$HOME/.local/share/mise/installs/go/1.27.1/bin
present_ips() {
  local ip held
  held=$(ip -4 -o addr show | awk '{print $4}' | cut -d/ -f1)
  for ip in $LAN_IPS; do grep -qx "$ip" <<<"$held" && printf '%s ' "$ip"; done
}
json() { python3 -c "import json,sys; $1"; }
src_jm() { docker exec fl-src /jellymesh "$@"; }
dst_jm() { docker exec fl-dst /jellymesh "$@"; }

build() {
  docker build -q -t jellymesh:fuselab "$root" >/dev/null
  (cd "$here" && CGO_ENABLED=0 PATH=$GO:$PATH go build -trimpath -o bin/faultproxy ./faultproxy)
  printf 'FROM alpine:3\nCOPY bin/faultproxy /usr/local/bin/\nENTRYPOINT ["/usr/local/bin/faultproxy"]\n' |
    docker build -q -t fuselab-proxy:latest -f - "$here" >/dev/null
}

# media links the three real films, and SYNTHETIC copies of them under other
# titles and identities, into a Jellyfin layout, each with an NFO, so the
# source identifies them without the internet.
media() {
  python3 - "$FILMS" "$BASE/media/Movies" "$SYNTHETIC" <<'PY'
import os, sys
films, out, synthetic = sys.argv[1], sys.argv[2], int(sys.argv[3])
real = [("Back to the Future", 1985, "105", "tt0088763", "Back To The Future (1985) Remastered.mkv"),
        ("Back to the Future Part II", 1989, "165", "tt0096874", "Back To The Future Part II (1989) Remastered.mkv"),
        ("Back to the Future Part III", 1990, "196", "tt0099088", "Back To The Future Part III (1990) Remastered.mkv")]
works = list(real) + [(f"Spike Film {n + 1:02d}", 2000 + n, str(900100 + n), f"tt99{n:05d}", real[n % 3][4]) for n in range(synthetic)]
for title, year, tmdb, imdb, file in works:
    folder = os.path.join(out, f"{title} ({year})")
    os.makedirs(folder, exist_ok=True)
    target = os.path.join(folder, f"{title} ({year}).mkv")
    if not os.path.exists(target):
        os.link(os.path.join(films, file), target)
    with open(os.path.join(folder, "movie.nfo"), "w") as handle:
        handle.write(f'<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<movie><title>{title}</title><year>{year}</year>'
                     f'<uniqueid type="tmdb" default="true">{tmdb}</uniqueid><tmdbid>{tmdb}</tmdbid>'
                     f'<uniqueid type="imdb">{imdb}</uniqueid><imdbid>{imdb}</imdbid><lockdata>true</lockdata></movie>\n')
print(f"media: {len(works)} films")
PY
}

start_mount() {
  docker run -d --name fl-mount --restart unless-stopped --user 0:0 --network none \
    --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined --memory 256m \
    --mount "type=bind,src=$BASE/share,dst=/share,bind-propagation=rshared" \
    -v "$BASE/generated:/generated:ro" -v fl-run:/run/jellymesh \
    -e JELLYMESH_MOUNT_POINT=/share/films -e JELLYMESH_MOUNT_ALLOW_UIDS=$JF_UID -e JELLYMESH_MOUNT_TEST_FAULTS=1 \
    --no-healthcheck jellymesh:fuselab mount >/dev/null
}

start_dst() {
  docker run -d --name fl-dst --restart unless-stopped --network fuselab --memory 1g -e GOMEMLIMIT=600MiB \
    -v fl-dst-data:/data -v fl-run:/run/jellymesh -v "$BASE/generated:/generated" \
    -e JELLYMESH_NODE_NAME=dst -e JELLYMESH_PUBLIC_HOSTNAME=fl-dst:8443 -e JELLYMESH_DIRECT_LISTEN_ADDR=off \
    -e JELLYMESH_JELLYFIN_URL=http://fl-dst-jf:8096 -e JELLYMESH_JELLYFIN_USER=jellymesh \
    -e JELLYMESH_JELLYFIN_PASSWORD="$(python3 "$here/setup.py" service "$BASE" destination)" \
    -e JELLYMESH_GENERATED_ROOT=/generated -e JELLYMESH_PRESENTATION=fuse jellymesh:fuselab >/dev/null
}

start_dst_jf() {
  docker run -d --name fl-dst-jf --restart unless-stopped --network fuselab --user "$JF_UID:$JF_UID" --memory 2g \
    -p 127.0.0.1:18230:8096 $(for ip in $(present_ips); do printf -- '-p %s:18230:8096 ' "$ip"; done) \
    -v fl-dst-jf-config:/config -v fl-dst-jf-cache:/cache \
    --mount "type=bind,src=$BASE/share,dst=/remote,readonly,bind-propagation=rslave" jellyfin/jellyfin:10.11.11 >/dev/null
}

up() {
  build
  mkdir -p "$BASE/state" "$BASE/share" "$BASE/generated"
  media
  docker network create fuselab >/dev/null
  # The node writes the generated root as uid 65532; Jellyfin's volumes are 7777's.
  docker volume create fl-dst-jf-config >/dev/null; docker volume create fl-dst-jf-cache >/dev/null
  docker run --rm -v "$BASE/generated:/g" -v fl-dst-jf-config:/c -v fl-dst-jf-cache:/k alpine:3 \
    sh -c "chown 65532:65532 /g && chown -R $JF_UID:$JF_UID /c /k" >/dev/null

  docker run -d --name fl-src-jf --restart unless-stopped --network fuselab --user "$(id -u):$(id -g)" --memory 2g \
    -p 127.0.0.1:18220:8096 -v fl-src-jf-config:/config -v "$BASE/media/Movies:/media/movies:ro" jellyfin/jellyfin:10.11.11 >/dev/null
  python3 "$here/setup.py" source http://127.0.0.1:18220 "$BASE"
  docker run -d --name fl-proxy --restart unless-stopped --network fuselab -p 127.0.0.1:18310:8300 fuselab-proxy:latest >/dev/null
  docker run -d --name fl-src --restart unless-stopped --network fuselab -v fl-src-data:/data \
    -e JELLYMESH_NODE_NAME=src -e JELLYMESH_PUBLIC_HOSTNAME=fl-src:8443 -e JELLYMESH_DIRECT_LISTEN_ADDR=off \
    -e JELLYMESH_UPLOAD_CEILING_MBPS=0 \
    -e JELLYMESH_JELLYFIN_URL=http://fl-proxy:8096 -e JELLYMESH_JELLYFIN_USER=jellymesh \
    -e JELLYMESH_JELLYFIN_PASSWORD="$(python3 "$here/setup.py" service "$BASE" source)" jellymesh:fuselab >/dev/null

  start_dst_jf
  python3 "$here/setup.py" destination http://127.0.0.1:18230 "$BASE"
  start_dst
  start_mount
  for _ in $(seq 1 30); do src_jm status >/dev/null 2>&1 && dst_jm status >/dev/null 2>&1 && break; sleep 1; done

  dst_jm found fuselab >/dev/null
  library=$(src_jm libraries | json 'print([l.get("id") or l.get("Id") for l in json.load(sys.stdin) if (l.get("name") or l.get("Name"))=="Movies"][0])')
  src_jm publish "$library" -root /media/movies >/dev/null
  code=$(dst_jm invite -valid-for 1h | json 'print(json.load(sys.stdin)["short_code"])')
  src_jm join -address fl-dst:8443 -code "$code" -wait 2m >"$BASE/state/join.log" 2>&1 &
  join=$!
  for _ in $(seq 1 20); do
    requests=$(dst_jm requests)
    [ "$(echo "$requests" | json 'print(len(json.load(sys.stdin) or []))')" -ge 1 ] && break; sleep 1
  done
  dst_jm approve "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["inviter_id"])')" \
    "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["invitation_id"])')" >/dev/null
  wait "$join"
  src_jm catalog-sync >/dev/null
  echo "materialized: $(dst_jm catalog-sync | json 'print(json.load(sys.stdin)["materialized"]["Written"])')"
  for _ in $(seq 1 30); do [ -d "$BASE/share/films/Movies" ] && break; sleep 1; done
  echo "films in the mount: $(ls "$BASE/share/films/Movies" | wc -l)"
  python3 "$here/setup.py" libraries http://127.0.0.1:18230 "$BASE"
}

down() {
  docker rm -f fl-dst-jf fl-mount fl-dst fl-src fl-proxy fl-src-jf >/dev/null 2>&1 || true
  docker volume rm fl-run fl-dst-data fl-src-data fl-src-jf-config fl-dst-jf-config fl-dst-jf-cache >/dev/null 2>&1 || true
  docker network rm fuselab >/dev/null 2>&1 || true
  # A dead mount can outlive its container; detach it from a privileged helper,
  # and remove what uid 65532 and 7777 own.
  if [ -d "$BASE" ]; then
    docker run --rm --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
      --mount "type=bind,src=$BASE,dst=/base,bind-propagation=rshared" alpine:3 \
      sh -c 'umount -l /base/share/films 2>/dev/null; rm -rf /base/generated /base/share' >/dev/null 2>&1 || true
    rm -rf "$BASE"
  fi
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  build) build ;;
  start_mount) start_mount ;;
  start_dst) start_dst ;;
  *) echo "usage: env.sh up|down|build"; exit 2 ;;
esac
