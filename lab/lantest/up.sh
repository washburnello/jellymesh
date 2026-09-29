#!/usr/bin/env bash
# The LAN test (conformance M-11): walnut publishes a real library from its
# own Jellyfin, and cedar's production Jellyfin consumes it through
# Jellymesh, over the LAN. Run it on walnut; it drives cedar over SSH.
#
# walnut: a Jellyfin on 127.0.0.1:18120 with the media directory read-only,
#         a non-administrator service user, and a Jellymesh node whose
#         federation port is on walnut's LAN address only.
# cedar:  a Jellymesh node on host networking, federation on cedar's LAN
#         address, relay and admin API on loopback (8090 and 8091 are taken
#         there), a generated root under Jellyfin's existing /media mount so
#         Jellyfin needs no restart, and a "Jellymesh Test" library over it.
#         cedar founds the group, since a founder need not publish.
#
# Passwords are random and kept only in ~/.config/jellymesh-lantest (0600).
# Everything is removed by down.sh.
#
#   ./lab/lantest/up.sh "$HOME/Videos/Back to the Future"
set -euo pipefail
media=${1:?give the media directory walnut publishes}
root=$(cd "$(dirname "$0")/../.." && pwd)
walnut_ip=${WALNUT_IP:-192.168.87.20}  # walnut's Wi-Fi address; its wired one has no route of its own
cedar_ip=${CEDAR_IP:-192.168.87.247}
cedar=${CEDAR_HOST:-cedar}
state="$HOME/.config/jellymesh-lantest"
cedar_generated=/mnt/tb01/jellyfin/media/jellymesh-test
report() { printf '%-52s %s\n' "$1" "$2"; }
json() { local code=$1; shift; python3 -c "import sys,json; $code" "$@"; }
mkdir -p "$state" && chmod 700 "$state"

# Jellyfin finds each film's identifiers in a sidecar NFO, so nothing depends
# on an online lookup matching "Remastered" titles.
nfo() { # file title year tmdb imdb
  local target="$media/${1%.*}.nfo"
  [ -e "$target" ] || printf '<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<movie><title>%s</title><year>%s</year><uniqueid type="tmdb" default="true">%s</uniqueid><tmdbid>%s</tmdbid><uniqueid type="imdb">%s</uniqueid><imdbid>%s</imdbid></movie>\n' "$2" "$3" "$4" "$4" "$5" "$5" > "$target"
}
nfo "Back To The Future (1985) Remastered.mkv" "Back to the Future" 1985 105 tt0088763
nfo "Back To The Future Part II (1989) Remastered.mkv" "Back to the Future Part II" 1989 165 tt0096874
nfo "Back To The Future Part III (1990) Remastered.mkv" "Back to the Future Part III" 1990 196 tt0099088

echo "building the jellymesh image and loading it on cedar"
docker build -q -t jellymesh:lan "$root" >/dev/null
docker save jellymesh:lan | ssh "$cedar" docker load >/dev/null

# --- walnut's Jellyfin --------------------------------------------------------
docker network create jm-lan >/dev/null
mkdir -p "$state/jellyfin-config"
docker run -d --name jm-lan-jellyfin --restart unless-stopped --network jm-lan --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:18120:8096 -v "$state/jellyfin-config:/config" -v "$media:/media/movies:ro" jellyfin/jellyfin:10.11.11 >/dev/null
url=http://127.0.0.1:18120
setup='Authorization: MediaBrowser Client="lantest", Device="lantest", DeviceId="lantest-setup", Version="1"'
admin_password=$(openssl rand -hex 16); service_password=$(openssl rand -hex 16)
for _ in $(seq 1 90); do curl -sf "$url/Startup/Configuration" -H "$setup" >/dev/null 2>&1 && break; sleep 2; done
curl -sf -X POST "$url/Startup/Configuration" -H "$setup" -H 'Content-Type: application/json' -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
curl -sf "$url/Startup/User" -H "$setup" >/dev/null
curl -sf -X POST "$url/Startup/User" -H "$setup" -H 'Content-Type: application/json' -d "{\"Name\":\"admin\",\"Password\":\"$admin_password\"}"
curl -sf -X POST "$url/Startup/Complete" -H "$setup"
umask 077
printf 'walnut Jellyfin %s\nadmin: admin / %s\nservice user: jellymesh / %s\n' "$url" "$admin_password" "$service_password" > "$state/credentials"
token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' -d "{\"Username\":\"admin\",\"Pw\":\"$admin_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
admin="$setup, Token=\"$token\""
curl -sf -X POST "$url/Library/VirtualFolders?name=Movies&collectionType=movies&paths=%2Fmedia%2Fmovies&refreshLibrary=true" \
  -H "$admin" -H 'Content-Type: application/json' -d '{"LibraryOptions":{"EnableRealtimeMonitor":false}}'
library=$(curl -sf "$url/Library/VirtualFolders" -H "$admin" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Movies"][0])')
user=$(curl -sf -X POST "$url/Users/New" -H "$admin" -H 'Content-Type: application/json' -d "{\"Name\":\"jellymesh\",\"Password\":\"$service_password\"}" | json 'print(json.load(sys.stdin)["Id"])')
policy=$(curl -sf "$url/Users/$user" -H "$admin" | json "p=json.load(sys.stdin)['Policy']; p['EnableAllFolders']=False; p['EnabledFolders']=['$library']; print(json.dumps(p))")
curl -sf -X POST "$url/Users/$user/Policy" -H "$admin" -H 'Content-Type: application/json' -d "$policy"
for _ in $(seq 1 60); do
  n=$(curl -sf "$url/Items?Recursive=true&IncludeItemTypes=Movie&Fields=ProviderIds" -H "$admin" | json 'print(sum(1 for i in json.load(sys.stdin)["Items"] if i.get("ProviderIds",{}).get("Tmdb")))')
  [ "$n" -ge 3 ] && break; sleep 3
done
report "walnut Jellyfin films with a TMDB identifier" "$n"

# --- the nodes ----------------------------------------------------------------
docker run -d --name jm-lan-walnut --restart unless-stopped --network jm-lan -v jm-lan-walnut-data:/data \
  -p "$walnut_ip:18443:8443" -p "$walnut_ip:44843:44843/udp" \
  -e JELLYMESH_NODE_NAME=walnut -e JELLYMESH_PUBLIC_HOSTNAME="$walnut_ip:18443" \
  -e JELLYMESH_DIRECT_CANDIDATES="$walnut_ip:44843" \
  -e JELLYMESH_JELLYFIN_URL=http://jm-lan-jellyfin:8096 -e JELLYMESH_JELLYFIN_USER=jellymesh -e JELLYMESH_JELLYFIN_PASSWORD="$service_password" \
  jellymesh:lan >/dev/null
ssh "$cedar" "sudo mkdir -p '$cedar_generated/Movies' && sudo chown -R 65532:65532 '$cedar_generated' && sudo chmod 0755 '$cedar_generated' '$cedar_generated/Movies'
  docker run -d --name jm-lan-cedar --restart unless-stopped --network host -v jm-lan-cedar-data:/data -v '$cedar_generated:/generated' \
    -e JELLYMESH_NODE_NAME=cedar -e JELLYMESH_PUBLIC_HOSTNAME='$cedar_ip:18443' \
    -e JELLYMESH_FEDERATION_LISTEN_ADDR='$cedar_ip:18443' -e JELLYMESH_ADMIN_LISTEN_ADDR=127.0.0.1:18191 \
    -e JELLYMESH_RELAY_LISTEN_ADDR=127.0.0.1:18190 -e JELLYMESH_RELAY_URL=http://127.0.0.1:18190 \
    -e JELLYMESH_GENERATED_ROOT=/generated -e JELLYMESH_JELLYFIN_GENERATED_ROOT=/media/jellymesh-test \
    -e JELLYMESH_DIRECT_CANDIDATES='$cedar_ip:44843' \
    jellymesh:lan >/dev/null"
walnut_jm() { docker exec jm-lan-walnut /jellymesh "$@"; }
cedar_jm() { ssh "$cedar" docker exec jm-lan-cedar /jellymesh "$@"; }
for _ in $(seq 1 30); do walnut_jm status >/dev/null 2>&1 && cedar_jm status >/dev/null 2>&1 && break; sleep 1; done
report "cedar reaches walnut's federation port" "$(ssh "$cedar" "timeout 5 bash -c '</dev/tcp/$walnut_ip/18443' && echo yes || echo NO")"
report "walnut reaches cedar's federation port" "$(timeout 5 bash -c "</dev/tcp/$cedar_ip/18443" && echo yes || echo NO)"

# --- the group ------------------------------------------------------------------
cedar_jm found lan-test >/dev/null
walnut_library=$(walnut_jm libraries | json 'print([l.get("id") or l.get("Id") for l in json.load(sys.stdin) if (l.get("name") or l.get("Name"))=="Movies"][0])')
walnut_jm publish "$walnut_library" -root /media/movies >/dev/null
code=$(cedar_jm invite -valid-for 1h | json 'print(json.load(sys.stdin)["short_code"])')
walnut_jm join -address "$cedar_ip:18443" -code "$code" -wait 2m >"$state/join.log" 2>&1 &
join=$!
for _ in $(seq 1 20); do
  requests=$(cedar_jm requests)
  [ "$(echo "$requests" | json 'print(len(json.load(sys.stdin) or []))')" -ge 1 ] && break; sleep 1
done
cedar_jm approve "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["inviter_id"])')" "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["invitation_id"])')" >/dev/null
wait "$join" && report "walnut joined cedar's group" "yes"
walnut_jm catalog-sync >/dev/null
report "items cedar materialized" "$(cedar_jm catalog-sync | json 'print(json.load(sys.stdin)["materialized"]["Written"])')"
report "cedar's generated files" "$(ssh "$cedar" "cd '$cedar_generated' && find . -type f | wc -l")"

# --- cedar's Jellyfin -----------------------------------------------------------
# The library reads generated files and must never write into them.
ssh "$cedar" 'bash -s' <<'EOF'
python3 - <<'PY'
import json,os,urllib.request,urllib.parse
cfg=json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key=next(v for k,v in cfg.items() if "key" in k.lower() or "token" in k.lower())
def call(method,path,query=None,body=None):
    url="http://127.0.0.1:8097"+path+("?"+urllib.parse.urlencode(query,doseq=True) if query else "")
    request=urllib.request.Request(url,method=method,data=json.dumps(body).encode() if body is not None else None,
        headers={"X-Emby-Token":key,"Content-Type":"application/json"})
    raw=urllib.request.urlopen(request,timeout=60).read()
    return json.loads(raw) if raw else None
if not any(l["Name"]=="Jellymesh Test" for l in call("GET","/Library/VirtualFolders")):
    call("POST","/Library/VirtualFolders",{"name":"Jellymesh Test","collectionType":"movies","paths":["/media/jellymesh-test/Movies"],"refreshLibrary":"false"},
         {"LibraryOptions":{"EnableRealtimeMonitor":False,"SaveLocalMetadata":False,"MetadataSavers":[]}})
call("POST","/Library/Refresh")
print("cedar Jellyfin: library created and a scan started")
PY
EOF
echo "credentials: $state/credentials"
