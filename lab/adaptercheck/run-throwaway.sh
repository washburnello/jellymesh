#!/usr/bin/env bash
# Runs the adapter check (conformance.md M-7) against a throwaway Jellyfin.
#
# It starts a fresh jellyfin/jellyfin:10.11.11 container on 127.0.0.1:18098
# with its own scratch data, creates two small libraries with every metadata
# fetcher off (so nothing is looked up online), creates a non-administrator
# service user granted those two libraries and not a third, runs
# ./lab/adaptercheck, and removes the container and its data. The generated
# passwords are never printed or stored.
#
# Media: two 2 MB truncated copies of the lab's test file are enough; the
# library scan is trivial.
#
#   ./lab/adaptercheck/run-throwaway.sh /path/to/any/test.mkv
set -euo pipefail
source_file=${1:?give a small test media file}
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
url=http://127.0.0.1:18098
cleanup() {
  docker rm -f jellymesh-m7 >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

mkdir -p "$work/config" "$work/media/movies/Probe Film (2001)" "$work/media/tv/Probe Show/Season 01"
head -c 2000000 "$source_file" > "$work/media/movies/Probe Film (2001)/Probe Film (2001).mkv"
head -c 2000000 "$source_file" > "$work/media/tv/Probe Show/Season 01/Probe Show S01E01.mkv"

admin_password=$(openssl rand -hex 16)
service_password=$(openssl rand -hex 16)
setup='Authorization: MediaBrowser Client="lab", Device="lab", DeviceId="m7-setup", Version="1"'

docker run -d --name jellymesh-m7 --user "$(id -u):$(id -g)" -p 127.0.0.1:18098:8096 \
  -v "$work/config:/config" -v "$work/media:/media:ro" jellyfin/jellyfin:10.11.11 >/dev/null
# /System/Info/Public answers before startup finishes; the setup routes
# return 503 until it has.
for _ in $(seq 1 90); do curl -sf "$url/Startup/Configuration" -H "$setup" >/dev/null 2>&1 && break; sleep 2; done

json() { python3 -c "import sys,json; $1"; }
curl -sf -X POST "$url/Startup/Configuration" -H "$setup" -H 'Content-Type: application/json' \
  -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
curl -sf "$url/Startup/User" -H "$setup" >/dev/null
curl -sf -X POST "$url/Startup/User" -H "$setup" -H 'Content-Type: application/json' -d "{\"Name\":\"labadmin\",\"Password\":\"$admin_password\"}"
curl -sf -X POST "$url/Startup/Complete" -H "$setup"
token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' \
  -d "{\"Username\":\"labadmin\",\"Pw\":\"$admin_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
admin="$setup, Token=\"$token\""

options='{"LibraryOptions":{"EnableRealtimeMonitor":false,"EnableChapterImageExtraction":false,"ExtractChapterImagesDuringLibraryScan":false,"MetadataSavers":[],"TypeOptions":[{"Type":"Movie","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Series","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Season","MetadataFetchers":[],"ImageFetchers":[]},{"Type":"Episode","MetadataFetchers":[],"ImageFetchers":[]}]}}'
for library in "Movies movies %2Fmedia%2Fmovies" "Shows tvshows %2Fmedia%2Ftv" "Hidden movies %2Fmedia%2Fmovies"; do
  set -- $library
  curl -sf -X POST "$url/Library/VirtualFolders?name=$1&collectionType=$2&paths=$3&refreshLibrary=false" \
    -H "$admin" -H 'Content-Type: application/json' -d "$options"
done
curl -sf -X POST "$url/Library/Refresh" -H "$admin"

libraries=$(curl -sf "$url/Library/VirtualFolders" -H "$admin")
granted=$(echo "$libraries" | json 'print(",".join(l["ItemId"] for l in json.load(sys.stdin) if l["Name"] in ("Movies","Shows")))')
user=$(curl -sf -X POST "$url/Users/New" -H "$admin" -H 'Content-Type: application/json' \
  -d "{\"Name\":\"jellymesh\",\"Password\":\"$service_password\"}" | json 'print(json.load(sys.stdin)["Id"])')
policy=$(curl -sf "$url/Users/$user" -H "$admin" | json "p=json.load(sys.stdin)['Policy']; p['EnableAllFolders']=False; p['EnabledFolders']='$granted'.split(','); p['IsAdministrator']=False; print(json.dumps(p))")
curl -sf -X POST "$url/Users/$user/Policy" -H "$admin" -H 'Content-Type: application/json' -d "$policy"

for _ in $(seq 1 60); do
  found=$(curl -sf "$url/Items?Recursive=true&IncludeItemTypes=Movie,Episode" -H "$admin" | json 'print(json.load(sys.stdin)["TotalRecordCount"])')
  [ "$found" -ge 2 ] && break; sleep 2
done
echo "scan found $found media items"

cd "$root"
JF_URL=$url JF_USER=jellymesh JF_PASSWORD=$service_password go run ./lab/adaptercheck
