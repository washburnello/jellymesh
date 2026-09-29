#!/usr/bin/env bash
# The FUSE spike's environment (#60), on walnut only.
#
#   env.sh up <films-dir>   build, generate state, start everything, set up Jellyfin
#   env.sh down             remove every container, volume, and mount
#
# Containers (network fusespike):
#   fusespike-source    stands in for a remote source; fault control on 127.0.0.1:18300
#   fusespike-reader    the daemon side: chunked range fetches; stats on 127.0.0.1:18200
#   fusespike-mount     the FUSE mount; the only container with /dev/fuse and SYS_ADMIN
#   fusespike-jellyfin  Jellyfin 10.11.11 as uid 7777, on 127.0.0.1:18130 and the LAN
#
# The mount is made at $BASE/share/mnt with shared propagation and reaches
# Jellyfin through a slave bind of $BASE/share, so a remount after a crash
# reappears in Jellyfin without restarting it.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
BASE=${FUSESPIKE_BASE:-$HOME/.local/share/jellymesh-fusespike}
LAN_IP=${LAN_IP:-192.168.87.249}
JF_UID=7777
url=http://127.0.0.1:18130

down() {
  docker rm -f fusespike-jellyfin fusespike-mount fusespike-reader fusespike-source >/dev/null 2>&1 || true
  docker volume rm fusespike-jf-config fusespike-jf-cache >/dev/null 2>&1 || true
  docker network rm fusespike >/dev/null 2>&1 || true
  # A dead mount can outlive its container; detach it from a privileged helper.
  docker run --rm --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
    --mount "type=bind,src=$BASE/share,dst=/share,bind-propagation=rshared" alpine:3 \
    sh -c 'umount -l /share/mnt 2>/dev/null; true' >/dev/null 2>&1 || true
  rm -rf "$BASE"
}

start_mount() {
  docker run -d --name fusespike-mount --restart unless-stopped --network fusespike \
    --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined --memory 256m \
    --mount "type=bind,src=$BASE/share,dst=/share,bind-propagation=rshared" -v "$BASE/state:/state:ro" \
    fusespike:latest /usr/local/bin/mount -allow-uids "$JF_UID" >/dev/null
}
start_reader() {
  docker run -d --name fusespike-reader --restart unless-stopped --network fusespike --memory 1g \
    -p 127.0.0.1:18200:8200 -v "$BASE/state:/state:ro" fusespike:latest /usr/local/bin/reader >/dev/null
}

# go-fuse does not yet send the kernel a request timeout; a small patch does
# (patches/go-fuse-request-timeout.patch), applied to a local copy.
prepare_gofuse() {
  local gopath=$HOME/.local/share/mise/installs/go/1.27.1/bin
  [ -d "$here/third_party/go-fuse" ] && return
  local module
  module=$(cd "$here" && PATH=$gopath:$PATH GOFLAGS=-mod=mod go list -m -f '{{.Dir}}' github.com/hanwen/go-fuse/v2@v2.11.0 2>/dev/null) ||
    module=$(PATH=$gopath:$PATH go env GOMODCACHE)/github.com/hanwen/go-fuse/v2@v2.11.0
  mkdir -p "$here/third_party" && cp -r "$module" "$here/third_party/go-fuse" && chmod -R u+w "$here/third_party/go-fuse"
  (cd "$here/third_party" && patch -p0 < "$here/patches/go-fuse-request-timeout.patch" >/dev/null)
}

build() {
  prepare_gofuse
  (cd "$here" && CGO_ENABLED=0 PATH=$HOME/.local/share/mise/installs/go/1.27.1/bin:$PATH go build -trimpath -o bin/ ./cmd/...)
  printf 'FROM alpine:3\nCOPY bin/ /usr/local/bin/\n' | docker build -q -t fusespike:latest -f - "$here" >/dev/null
}

up() {
  local films=${1:?give the films directory}
  build
  mkdir -p "$BASE/state" "$BASE/share" "$BASE/strm"
  python3 "$here/state.py" "$films" "$BASE"
  docker network create fusespike >/dev/null
  docker run -d --name fusespike-source --restart unless-stopped --network fusespike --memory 256m \
    -p 127.0.0.1:18300:8300 -v "$films:/films:ro" fusespike:latest /usr/local/bin/source >/dev/null
  start_reader
  start_mount
  for _ in $(seq 1 20); do [ -d "$BASE/share/mnt/Movies" ] && break; sleep 0.5; done
  [ -d "$BASE/share/mnt/Movies" ] || { echo "mount did not appear"; docker logs fusespike-mount; exit 1; }
  docker volume create fusespike-jf-config >/dev/null; docker volume create fusespike-jf-cache >/dev/null
  docker run --rm -v fusespike-jf-config:/c -v fusespike-jf-cache:/k alpine:3 chown -R "$JF_UID:$JF_UID" /c /k
  chmod -R a+rX "$BASE/strm"
  docker run -d --name fusespike-jellyfin --restart unless-stopped --network fusespike --user "$JF_UID:$JF_UID" --memory 2g \
    -p "127.0.0.1:18130:8096" -p "$LAN_IP:18130:8096" \
    -v fusespike-jf-config:/config -v fusespike-jf-cache:/cache \
    --mount "type=bind,src=$BASE/share,dst=/remote,readonly,bind-propagation=rslave" \
    -v "$BASE/strm:/strm:ro" jellyfin/jellyfin:10.11.11 >/dev/null
  python3 "$here/jellyfin_setup.py" "$url" "$BASE"
}

case "${1:-}" in
  up) shift; up "$@" ;;
  build) build ;;
  down) down ;;
  start_mount) start_mount ;;
  start_reader) start_reader ;;
  *) echo "usage: env.sh up <films-dir> | down"; exit 2 ;;
esac
