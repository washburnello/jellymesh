#!/usr/bin/env bash
# Probes how Jellyfin 10.11.11 keys per-user state for generated items, the
# Phase 4 design inputs (conformance M-10): whether watched state, resume
# position, and favourites survive an item being removed and returning under
# another folder and source; whether a generated copy shares state with a
# local copy of the same work; and whether episode files from two sources
# become versions of one episode.
#
# It starts a throwaway container on 127.0.0.1:18102, with metadata fetchers
# off and random credentials that are never printed, and removes everything
# afterwards. Scans are started as the lab administrator, standing in for
# Jellyfin's scheduled scan (A-12).
#
#   ./lab/identitycheck/run-throwaway.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
cleanup() { docker rm -f jellymesh-m10 >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

mkdir -p "$work/config" "$work/media"
ffmpeg -loglevel error -f lavfi -i testsrc=size=160x90:rate=5 -t 4 -c:v libx264 -preset ultrafast "$work/clip.mkv"
docker run -d --name jellymesh-m10 --user "$(id -u):$(id -g)" -p 127.0.0.1:18102:8096 \
  -v "$work/config:/config" -v "$work/media:/media" jellyfin/jellyfin:10.11.11 >/dev/null
python3 "$here/probe.py" http://127.0.0.1:18102 "$work/media" /media "$work/clip.mkv"
