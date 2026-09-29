#!/usr/bin/env bash
# Removes everything up.sh made, on both machines, letting cedar's Jellyfin
# drop the generated films before their library goes. It does not touch the
# media, and it does not move anything back into cedar's library; see
# restore.sh, which should run after this.
#
# Order matters on cedar. Jellyfin removes a library's missing items only in
# a scan of that library, and skips a root that is completely empty, treating
# it as unmounted. So the generated files go, a placeholder folder keeps the
# root non-empty, a scan drops the items (detaching users' state so that it
# can reattach to the originals, A-14), and only then do the library and its
# folder go. Removing the library first leaves the items orphaned.
#
#   ./lab/lantest/down.sh
set -uo pipefail
cedar=${CEDAR_HOST:-cedar}
docker rm -f jm-lan-walnut jm-lan-jellyfin >/dev/null 2>&1
docker rm -f jm-lan-walnut-old >/dev/null 2>&1
docker volume rm jm-lan-walnut-data jm-lan-walnut-restored jm-lan-walnut-rebuilt >/dev/null 2>&1
docker network rm jm-lan >/dev/null 2>&1
rm -rf "$HOME/.config/jellymesh-lantest"
ssh "$cedar" 'bash -s' <<'REMOTE'
generated=/mnt/tb01/jellyfin/media/jellymesh-test
docker rm -f jm-lan-cedar >/dev/null 2>&1
docker volume rm jm-lan-cedar-data >/dev/null 2>&1
docker image rm jellymesh:lan >/dev/null 2>&1
if [ -d "$generated/Movies" ]; then
  sudo find "$generated/Movies" -mindepth 1 -delete
  sudo mkdir -p "$generated/Movies/placeholder"
fi
python3 - <<'PY'
import json, os, time, urllib.request, urllib.parse
cfg = json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key = next(v for k, v in cfg.items() if "key" in k.lower() or "token" in k.lower())
def call(method, path, query=None):
    url = "http://127.0.0.1:8097" + path + ("?" + urllib.parse.urlencode(query) if query else "")
    raw = urllib.request.urlopen(urllib.request.Request(url, method=method, headers={"X-Emby-Token": key}), timeout=60).read()
    return json.loads(raw) if raw else None
def generated_items():
    items = call("GET", "/Items", {"Recursive": "true", "IncludeItemTypes": "Movie,Episode", "Fields": "Path"})["Items"]
    return [i for i in items if (i.get("Path") or "").startswith("/media/jellymesh-test/")]
if not any(l["Name"] == "Jellymesh Test" for l in call("GET", "/Library/VirtualFolders")):
    raise SystemExit("no Jellymesh Test library")
call("POST", "/Library/Refresh")
time.sleep(5)
for _ in range(120):
    task = [t for t in call("GET", "/ScheduledTasks") if t.get("Key") == "RefreshLibrary"][0]
    if task["State"] == "Idle" and not generated_items():
        break
    time.sleep(5)
left = generated_items()
if left:
    raise SystemExit(f"{len(left)} generated items remain; the library and folder are kept so a later scan can drop them")
call("DELETE", "/Library/VirtualFolders", {"name": "Jellymesh Test"})
print("generated items gone; library removed")
PY
[ $? -eq 0 ] && sudo rm -rf "$generated"
REMOTE
echo "removed; the walnut media copy and cedar's ~/Videos copy are untouched"
