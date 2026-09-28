#!/usr/bin/env bash
# The whole playback chain against real Jellyfin 10.11.11 (conformance M-9).
#
# Two throwaway Jellyfins (a source and a destination) and two Jellymesh
# nodes from the current image run on an isolated Docker network. cedar
# publishes a film from the source Jellyfin; walnut, beside the destination
# Jellyfin, joins, materializes it, and serves it through its relay. The
# destination Jellyfin then streams it, and the bytes are compared with the
# source file. Credentials are random and never printed, metadata fetchers are
# off, and everything is removed afterwards.
#
#   ./lab/playcheck/run-throwaway.sh /path/to/small-test.mkv
set -euo pipefail
source_file=${1:?give a small test media file}
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
network=jm-m9
subnet=10.231.99.0/24
names=(jm-m9-source jm-m9-dest jm-m9-cedar jm-m9-walnut)
cleanup() {
  docker rm -f "${names[@]}" >/dev/null 2>&1 || true
  docker volume rm jm-m9-cedar-data jm-m9-walnut-data >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  # The generated files are owned by the Jellymesh container's user.
  docker run --rm --entrypoint rm -v "$work:/w" jellyfin/jellyfin:10.11.11 -rf /w/generated >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
report() { printf '%-60s %s\n' "$1" "$2"; }
json() { local code=$1; shift; python3 -c "import sys,json; $code" "$@"; }

mkdir -p "$work/source/Probe Film (2001)" "$work/docs/Oceans (2010)" "$work/generated/Movies" "$work/source-config" "$work/dest-config"
chmod 0777 "$work/generated" "$work/generated/Movies"
head -c 3000000 "$source_file" > "$work/source/Probe Film (2001)/Probe Film (2001).mkv"
printf '1\n00:00:01,000 --> 00:00:03,000\nHello\n' > "$work/source/Probe Film (2001)/Probe Film (2001).en.srt"
# The source's own NFO gives the film a clean title and a provider ID, as an
# online metadata fetch would; the identifier is synthetic and fetchers are off.
cat > "$work/source/Probe Film (2001)/movie.nfo" <<'NFO'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie><title>Probe Film</title><year>2001</year><tmdbid>999004</tmdbid><plot>A lab probe.</plot></movie>
NFO
head -c 1000000 "$source_file" > "$work/docs/Oceans (2010)/Oceans (2010).mkv"

echo "building the jellymesh image"
docker build -q -t jellymesh:m9 "$root" >/dev/null
docker network create --subnet "$subnet" "$network" >/dev/null

docker run -d --name jm-m9-source --network "$network" --user "$(id -u):$(id -g)" -p 127.0.0.1:18112:8096 \
  -v "$work/source-config:/config" -v "$work/source:/media/movies:ro" jellyfin/jellyfin:10.11.11 >/dev/null
docker run -d --name jm-m9-dest --network "$network" --user "$(id -u):$(id -g)" -p 127.0.0.1:18111:8096 \
  -v "$work/dest-config:/config" -v "$work/docs:/media/docs:ro" -v "$work/generated:/generated:ro" jellyfin/jellyfin:10.11.11 >/dev/null

setup='Authorization: MediaBrowser Client="lab", Device="lab", DeviceId="m9-setup", Version="1"'
options='{"LibraryOptions":{"EnableRealtimeMonitor":false,"MetadataSavers":[],"TypeOptions":[{"Type":"Movie","MetadataFetchers":[],"ImageFetchers":[]}]}}'
# bootstrap URL SERVICE_PASSWORD "Name|path|granted" ... -> prints the admin token
bootstrap() {
  local url=$1 service_password=$2; shift 2
  local admin_password; admin_password=$(openssl rand -hex 16)
  for _ in $(seq 1 90); do curl -sf "$url/Startup/Configuration" -H "$setup" >/dev/null 2>&1 && break; sleep 2; done
  curl -sf -X POST "$url/Startup/Configuration" -H "$setup" -H 'Content-Type: application/json' -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
  curl -sf "$url/Startup/User" -H "$setup" >/dev/null
  curl -sf -X POST "$url/Startup/User" -H "$setup" -H 'Content-Type: application/json' -d "{\"Name\":\"labadmin\",\"Password\":\"$admin_password\"}"
  curl -sf -X POST "$url/Startup/Complete" -H "$setup"
  local token; token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' -d "{\"Username\":\"labadmin\",\"Pw\":\"$admin_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
  local admin="$setup, Token=\"$token\"" granted=""
  for spec in "$@"; do
    IFS='|' read -r name path grant <<<"$spec"
    curl -sf -X POST "$url/Library/VirtualFolders?name=$name&collectionType=movies&paths=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=""))' "$path")&refreshLibrary=false" \
      -H "$admin" -H 'Content-Type: application/json' -d "$options"
  done
  curl -sf -X POST "$url/Library/Refresh" -H "$admin"
  for spec in "$@"; do
    IFS='|' read -r name path grant <<<"$spec"
    if [ "$grant" = yes ]; then
      granted="$granted,$(curl -sf "$url/Library/VirtualFolders" -H "$admin" | json "print([l['ItemId'] for l in json.load(sys.stdin) if l['Name']=='$name'][0])")"
    fi
  done
  local user; user=$(curl -sf -X POST "$url/Users/New" -H "$admin" -H 'Content-Type: application/json' -d "{\"Name\":\"jellymesh\",\"Password\":\"$service_password\"}" | json 'print(json.load(sys.stdin)["Id"])')
  local policy; policy=$(curl -sf "$url/Users/$user" -H "$admin" | json "p=json.load(sys.stdin)['Policy']; p['EnableAllFolders']=False; p['EnabledFolders']=[x for x in '$granted'.split(',') if x]; print(json.dumps(p))")
  curl -sf -X POST "$url/Users/$user/Policy" -H "$admin" -H 'Content-Type: application/json' -d "$policy"
  echo "$token"
}
cedar_service=$(openssl rand -hex 16); walnut_service=$(openssl rand -hex 16)
source_token=$(bootstrap http://127.0.0.1:18112 "$cedar_service" "Movies|/media/movies|yes")
dest_token=$(bootstrap http://127.0.0.1:18111 "$walnut_service" "Docs|/media/docs|yes" "Friends|/generated/Movies|no")
dest_admin="$setup, Token=\"$dest_token\""; source_admin="$setup, Token=\"$source_token\""
for _ in $(seq 1 40); do
  n=$(curl -sf "http://127.0.0.1:18112/Items?Recursive=true&IncludeItemTypes=Movie" -H "$source_admin" | json 'print(json.load(sys.stdin)["TotalRecordCount"])')
  [ "$n" -ge 1 ] && break; sleep 3
done
source_library=$(curl -sf http://127.0.0.1:18112/Library/VirtualFolders -H "$source_admin" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Movies"][0])')
docs_library=$(curl -sf http://127.0.0.1:18111/Library/VirtualFolders -H "$dest_admin" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Docs"][0])')

node() {
  local name=$1 jellyfin=$2 password=$3; shift 3
  docker volume create "jm-m9-$name-data" >/dev/null
  docker run -d --name "jm-m9-$name" --network "$network" -v "jm-m9-$name-data:/data" "$@" \
    -e JELLYMESH_NODE_NAME="$name" -e JELLYMESH_PUBLIC_HOSTNAME="jm-m9-$name:8443" \
    -e JELLYMESH_JELLYFIN_URL="$jellyfin" -e JELLYMESH_JELLYFIN_USER=jellymesh -e JELLYMESH_JELLYFIN_PASSWORD="$password" \
    jellymesh:m9 >/dev/null
}
node cedar http://jm-m9-source:8096 "$cedar_service"
node walnut http://jm-m9-dest:8096 "$walnut_service" -v "$work/generated:/generated" \
  -e JELLYMESH_GENERATED_ROOT=/generated -e JELLYMESH_RELAY_LISTEN_ADDR=0.0.0.0:8090 \
  -e JELLYMESH_RELAY_URL=http://jm-m9-walnut:8090 -e JELLYMESH_RELAY_ALLOWED_CLIENTS="$subnet"
cedar() { docker exec jm-m9-cedar /jellymesh "$@"; }
walnut() { docker exec jm-m9-walnut /jellymesh "$@"; }
for _ in $(seq 1 30); do cedar status >/dev/null 2>&1 && walnut status >/dev/null 2>&1 && break; sleep 1; done

cedar found m9-group >/dev/null
cedar publish "$source_library" -root /media/movies >/dev/null
walnut publish "$docs_library" -root /media/docs >/dev/null
invitation=$(cedar invite -valid-for 1h)
code=$(echo "$invitation" | json 'print(json.load(sys.stdin)["short_code"])')
walnut join -address jm-m9-cedar:8443 -code "$code" -wait 2m >"$work/join.log" 2>&1 &
join=$!
for _ in $(seq 1 20); do
  requests=$(cedar requests)
  [ "$(echo "$requests" | json 'print(len(json.load(sys.stdin) or []))')" -ge 1 ] && break; sleep 1
done
cedar approve "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["inviter_id"])')" "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["invitation_id"])')" >/dev/null
wait "$join" && report "walnut joined cedar's group" "yes"
cedar catalog-sync >/dev/null
materialized=$(walnut catalog-sync | json 'print(json.load(sys.stdin)["materialized"]["Written"])')
report "items walnut materialized" "$materialized"
report "generated files" "$(cd "$work/generated" && find . -type f | sed 's|^\./||' | tr '\n' ';')"

curl -sf -X POST http://127.0.0.1:18111/Library/Refresh -H "$dest_admin"
friends=$(curl -sf http://127.0.0.1:18111/Library/VirtualFolders -H "$dest_admin" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Friends"][0])')
for _ in $(seq 1 40); do
  found=$(curl -sf "http://127.0.0.1:18111/Items?Recursive=true&ParentId=$friends&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams,ProviderIds" -H "$dest_admin")
  [ "$(echo "$found" | json 'print(json.load(sys.stdin)["TotalRecordCount"])')" -ge 1 ] && break; sleep 3
done
# Jellyfin applies NFO metadata in a refresh after the item first appears.
for _ in $(seq 1 30); do
  found=$(curl -sf "http://127.0.0.1:18111/Items?Recursive=true&ParentId=$friends&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams,ProviderIds,Overview" -H "$dest_admin")
  [ "$(echo "$found" | json 'print(1 if json.load(sys.stdin)["Items"][0].get("ProviderIds",{}).get("Tmdb") else 0)')" = 1 ] && break; sleep 3
done
echo "$found" | json '
d=json.load(sys.stdin)
for it in d["Items"]:
    print("destination item: name=%r year=%s providers=%s overview=%r" % (it["Name"], it.get("ProductionYear"), it.get("ProviderIds"), it.get("Overview")))
    for ms in it.get("MediaSources", []):
        print("  version %r path=%s" % (ms.get("Name"), ms.get("Path")))
        for st in ms.get("MediaStreams", []):
            print("    stream %s %s %s external=%s" % (st.get("Type"), st.get("Codec"), st.get("Language"), st.get("IsExternal")))
'
item=$(echo "$found" | json 'print(json.load(sys.stdin)["Items"][0]["Id"])')
media_source=$(echo "$found" | json 'print(json.load(sys.stdin)["Items"][0]["MediaSources"][0]["Id"])')
info=$(curl -sf -X POST "http://127.0.0.1:18111/Items/$item/PlaybackInfo" -H "$dest_admin" -H 'Content-Type: application/json' -d '{}')
report "PlaybackInfo reveals a peer address" "$(echo "$info" | grep -c 'jm-m9-cedar' || true)"

expected=$(python3 -c "import sys;d=open(sys.argv[1],'rb').read()[1000000:1000100];print(d.hex())" "$work/source/Probe Film (2001)/Probe Film (2001).mkv")
got=$(curl -sf -H "$dest_admin" -H 'Range: bytes=1000000-1000099' "http://127.0.0.1:18111/Videos/$item/stream?static=true&mediaSourceId=$media_source" | python3 -c "import sys;print(sys.stdin.buffer.read().hex())")
report "Jellyfin streams the source's bytes through the relay (range)" "$([ "$expected" = "$got" ] && echo exact || echo MISMATCH)"
total=$(curl -sf -H "$dest_admin" "http://127.0.0.1:18111/Videos/$item/stream?static=true&mediaSourceId=$media_source" | wc -c)
report "whole-file stream length (source file is 3000000)" "$total"
