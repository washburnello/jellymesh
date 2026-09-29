#!/usr/bin/env bash
# Removes everything up.sh made, on both machines, and starts a scan on cedar
# so the generated films leave its library. It does not touch the media, and
# it does not move anything back into cedar's library; see restore.sh.
#
#   ./lab/lantest/down.sh
set -uo pipefail
cedar=${CEDAR_HOST:-cedar}
docker rm -f jm-lan-walnut jm-lan-jellyfin >/dev/null 2>&1
docker volume rm jm-lan-walnut-data >/dev/null 2>&1
docker network rm jm-lan >/dev/null 2>&1
rm -rf "$HOME/.config/jellymesh-lantest"
ssh "$cedar" 'bash -s' <<'EOF'
docker rm -f jm-lan-cedar >/dev/null 2>&1
docker volume rm jm-lan-cedar-data >/dev/null 2>&1
docker image rm jellymesh:lan >/dev/null 2>&1
sudo rm -rf /mnt/tb01/jellyfin/media/jellymesh-test
python3 - <<'PY'
import json,os,urllib.request,urllib.parse
cfg=json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key=next(v for k,v in cfg.items() if "key" in k.lower() or "token" in k.lower())
def call(method,path,query=None):
    url="http://127.0.0.1:8097"+path+("?"+urllib.parse.urlencode(query) if query else "")
    urllib.request.urlopen(urllib.request.Request(url,method=method,headers={"X-Emby-Token":key}),timeout=60).read()
# Removing a library entry leaves its folder's files alone; the folder is
# already gone.
try: call("DELETE","/Library/VirtualFolders",{"name":"Jellymesh Test"})
except Exception as error: print("library:",error)
call("POST","/Library/Refresh")
PY
EOF
echo "removed; the walnut media copy and cedar's ~/Videos copy are untouched"
