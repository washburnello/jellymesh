#!/usr/bin/env bash
# Probes how Jellyfin 10.11.11 treats Jellymesh's generated artifacts and
# which media routes a non-administrator can use (the Phase 3 design inputs).
# It starts a throwaway container on 127.0.0.1:18101, with metadata fetchers
# off and random credentials that are never printed, and removes everything
# afterwards.
#
#   ./lab/materializecheck/run-throwaway.sh /path/to/small-test.mkv
set -euo pipefail
source_file=${1:?give a small test media file}
work=$(mktemp -d)
url=http://127.0.0.1:18101
cleanup() { docker rm -f jellymesh-m8 >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

mkdir -p "$work/config" "$work/generated/Movies" "$work/source/Probe Film (2001)"
head -c 2000000 "$source_file" > "$work/source/Probe Film (2001)/Probe Film (2001).mkv"
printf '1\n00:00:01,000 --> 00:00:03,000\nHello\n' > "$work/source/Probe Film (2001)/Probe Film (2001).en.srt"

admin_password=$(openssl rand -hex 16); user_password=$(openssl rand -hex 16)
setup='Authorization: MediaBrowser Client="lab", Device="lab", DeviceId="m8-setup", Version="1"'
json() { local code=$1; shift; python3 -c "import sys,json; $code" "$@"; }
report() { printf '%-58s %s\n' "$1" "$2"; }

docker run -d --name jellymesh-m8 --user "$(id -u):$(id -g)" -p 127.0.0.1:18101:8096 \
  -v "$work/config:/config" -v "$work/generated:/generated" -v "$work/source:/source:ro" jellyfin/jellyfin:10.11.11 >/dev/null
for _ in $(seq 1 90); do curl -sf "$url/Startup/Configuration" -H "$setup" >/dev/null 2>&1 && break; sleep 2; done
curl -sf -X POST "$url/Startup/Configuration" -H "$setup" -H 'Content-Type: application/json' -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}'
curl -sf "$url/Startup/User" -H "$setup" >/dev/null
curl -sf -X POST "$url/Startup/User" -H "$setup" -H 'Content-Type: application/json' -d "{\"Name\":\"labadmin\",\"Password\":\"$admin_password\"}"
curl -sf -X POST "$url/Startup/Complete" -H "$setup"
token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' -d "{\"Username\":\"labadmin\",\"Pw\":\"$admin_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
admin="$setup, Token=\"$token\""
options='{"LibraryOptions":{"EnableRealtimeMonitor":true,"MetadataSavers":[],"TypeOptions":[{"Type":"Movie","MetadataFetchers":[],"ImageFetchers":[]}]}}'
curl -sf -X POST "$url/Library/VirtualFolders?name=Generated&collectionType=movies&paths=%2Fgenerated%2FMovies&refreshLibrary=false" -H "$admin" -H 'Content-Type: application/json' -d "$options"
curl -sf -X POST "$url/Library/VirtualFolders?name=Source&collectionType=movies&paths=%2Fsource&refreshLibrary=true" -H "$admin" -H 'Content-Type: application/json' -d "$options"
user=$(curl -sf -X POST "$url/Users/New" -H "$admin" -H 'Content-Type: application/json' -d "{\"Name\":\"service\",\"Password\":\"$user_password\"}" | json 'print(json.load(sys.stdin)["Id"])')
user_token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' -d "{\"Username\":\"service\",\"Pw\":\"$user_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
as_user="$setup, Token=\"$user_token\""
libraries=$(curl -sf "$url/Library/VirtualFolders" -H "$admin")
generated_id=$(echo "$libraries" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Generated"][0])')

# The real-time monitor may only watch libraries that existed at startup.
docker restart jellymesh-m8 >/dev/null
for _ in $(seq 1 90); do curl -sf "$url/System/Info" -H "$admin" >/dev/null 2>&1 && break; sleep 2; done
user_token=$(curl -sf -X POST "$url/Users/AuthenticateByName" -H "$setup" -H 'Content-Type: application/json' -d "{\"Username\":\"service\",\"Pw\":\"$user_password\"}" | json 'print(json.load(sys.stdin)["AccessToken"])')
as_user="$setup, Token=\"$user_token\""
sleep 10
echo "== what a non-administrator may trigger"
report "POST /Library/Refresh" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/Library/Refresh" -H "$as_user")"
report "POST /Library/Media/Updated" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/Library/Media/Updated" -H "$as_user" -H 'Content-Type: application/json' -d '{"Updates":[{"Path":"/generated/Movies","UpdateType":"Created"}]}')"
report "POST /Items/{library}/Refresh" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/Items/$generated_id/Refresh?Recursive=true" -H "$as_user")"

echo "== generated artifacts, detected by the real-time monitor"
dir="$work/generated/Movies/Generated Probe (2003) [jmid-4f2a9c1e]"
mkdir -p "$dir"
cat > "$dir/movie.nfo" <<'NFO'
<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<movie><title>Generated Probe</title><year>2003</year><tagline>A generated probe</tagline><rating>6.5</rating><criticrating>72</criticrating><studio>Probe Pictures</studio><tag>From Cedar</tag><tag>From Walnut</tag><uniqueid type="tmdb" default="true">999003</uniqueid><tmdbid>999003</tmdbid><lockdata>true</lockdata></movie>
NFO
echo "http://127.0.0.1:9/r/aaaa" > "$dir/.jellymesh-tmp-1.strm"
folder=$(basename "$dir")
echo "http://127.0.0.1:9/r/bbbb" > "$dir/$folder - Cedar.strm"
echo "http://127.0.0.1:9/r/cccc" > "$dir/$folder - Walnut.strm"
printf '1\n00:00:01,000 --> 00:00:03,000\nHi\n' > "$dir/$folder - Cedar.en.srt"
count_items() {
  curl -sf "$url/Items?Recursive=true&ParentId=$generated_id&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams,Path,ProviderIds,Tags,Studios,Taglines" -H "$admin"
}
wait_for_item() {
  local seconds=$1 start=$(date +%s)
  for _ in $(seq 1 $((seconds / 3))); do
    count_items > "$work/found.json"
    [ "$(json 'print(json.load(open(sys.argv[1]))["TotalRecordCount"])' "$work/found.json")" -ge 1 ] && { echo $(( $(date +%s) - start )); return; }
    sleep 3
  done
  echo "none after ${seconds}s"
}
report "real-time monitor alone" "$(wait_for_item 150)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$url/Library/Media/Updated" -H "$as_user" -H 'Content-Type: application/json' \
  -d "$(python3 -c "import json,sys; print(json.dumps({'Updates':[{'Path':'/generated/Movies/' + sys.argv[1], 'UpdateType':'Created'}]}))" "$(basename "$dir")")")
report "non-admin POST /Library/Media/Updated for the folder" "$code, detected: $(wait_for_item 90)"
if [ "$(json 'print(json.load(open(sys.argv[1]))["TotalRecordCount"])' "$work/found.json")" -eq 0 ]; then
  curl -sf -X POST "$url/Library/Refresh" -H "$admin"
  report "admin full refresh (does it parse at all?)" "$(wait_for_item 90)"
fi
json '
d=json.load(open(sys.argv[1]))
print("items:", d["TotalRecordCount"])
for it in d["Items"]:
    print("  name=%r year=%s providers=%s" % (it["Name"], it.get("ProductionYear"), it.get("ProviderIds")))
    print("  tags=%s studios=%s taglines=%s rating=%s critic=%s" % (it.get("Tags"), [s["Name"] for s in it.get("Studios", [])], it.get("Taglines"), it.get("CommunityRating"), it.get("CriticRating")))
    for ms in it.get("MediaSources", []):
        print("  version name=%r protocol=%s" % (ms.get("Name"), ms.get("Protocol")))
        for st in ms.get("MediaStreams", []):
            print("    stream type=%s codec=%s lang=%s external=%s" % (st.get("Type"), st.get("Codec"), st.get("Language"), st.get("IsExternal")))
' "$work/found.json"
echo "== removal"
rm -rf "$dir"; start=$(date +%s)
curl -s -o /dev/null -X POST "$url/Library/Media/Updated" -H "$as_user" -H 'Content-Type: application/json' \
  -d "$(python3 -c "import json,sys; print(json.dumps({'Updates':[{'Path':'/generated/Movies/' + sys.argv[1], 'UpdateType':'Deleted'}]}))" "$(basename "$dir")")"
for _ in $(seq 1 40); do
  n=$(curl -sf "$url/Items?Recursive=true&ParentId=$generated_id&IncludeItemTypes=Movie" -H "$admin" | json 'print(json.load(sys.stdin)["TotalRecordCount"])')
  [ "$n" -eq 0 ] && break; sleep 3
done
report "seconds until a removed folder left the library" "$(( $(date +%s) - start )) (remaining $n)"

echo "== source routes for a non-administrator"
# Grant the user the source library only, as a service user would be.
source_id=$(echo "$libraries" | json 'print([l["ItemId"] for l in json.load(sys.stdin) if l["Name"]=="Source"][0])')
policy=$(curl -sf "$url/Users/$user" -H "$admin" | json "p=json.load(sys.stdin)['Policy']; p['EnableAllFolders']=False; p['EnabledFolders']=['$source_id']; print(json.dumps(p))")
curl -sf -X POST "$url/Users/$user/Policy" -H "$admin" -H 'Content-Type: application/json' -d "$policy"
for _ in $(seq 1 40); do
  item=$(curl -sf "$url/Items?Recursive=true&ParentId=$source_id&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams" -H "$as_user")
  [ "$(echo "$item" | json 'print(json.load(sys.stdin)["TotalRecordCount"])')" -ge 1 ] && break; sleep 3
done
item_id=$(echo "$item" | json 'print(json.load(sys.stdin)["Items"][0]["Id"])')
media_source=$(echo "$item" | json 'print(json.load(sys.stdin)["Items"][0]["MediaSources"][0]["Id"])')
subtitle_index=$(echo "$item" | json 'ms=json.load(sys.stdin)["Items"][0]["MediaSources"][0]["MediaStreams"]; print([s["Index"] for s in ms if s["Type"]=="Subtitle"][0] if any(s["Type"]=="Subtitle" for s in ms) else -1)')
report "source item's external subtitle stream index" "$subtitle_index"
report "GET /Videos/{id}/stream?static=true, Range 0-99" "$(curl -s -o /dev/null -w '%{http_code} %{size_download} bytes' -H "$as_user" -H 'Range: bytes=0-99' "$url/Videos/$item_id/stream?static=true&mediaSourceId=$media_source")"
report "HEAD /Videos/{id}/stream?static=true" "$(curl -s -o /dev/null -I -w '%{http_code} length=%header{content-length} type=%header{content-type}' -H "$as_user" "$url/Videos/$item_id/stream?static=true&mediaSourceId=$media_source")"
report "GET subtitle stream as srt" "$(curl -s -o /dev/null -w '%{http_code} %{size_download} bytes' -H "$as_user" "$url/Videos/$item_id/$media_source/Subtitles/$subtitle_index/0/Stream.srt")"
report "GET /Items/{id}/Images/Primary" "$(curl -s -o /dev/null -w '%{http_code}' -H "$as_user" "$url/Items/$item_id/Images/Primary")"
report "GET /Items/{id}/Download" "$(curl -s -o /dev/null -w '%{http_code}' -H "$as_user" -H 'Range: bytes=0-99' "$url/Items/$item_id/Download")"
