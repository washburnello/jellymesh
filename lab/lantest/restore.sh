#!/usr/bin/env bash
# Puts the borrowed films back in cedar's Movies library after the LAN test.
# Run it after down.sh: the generated copies must leave Jellyfin first, so
# that the state Jellyfin moved onto them is detached again and reattaches to
# the originals when they return (conformance A-14). It copies from
# ~/Videos/Back to the Future on cedar, checks every file against the
# checksums taken when they were moved, puts back the collection file if the
# collection lost them, and starts a scan. The ~/Videos copy is kept.
#
#   ./lab/lantest/restore.sh
set -euo pipefail
ssh "${CEDAR_HOST:-cedar}" 'bash -s' <<'EOF'
set -euo pipefail
library=/mnt/tb01/jellyfin/media/Movies
from="$HOME/Videos/Back to the Future"
kit="$HOME/Videos/jellymesh-test-restore"
collection="/mnt/tb01/jellyfin/config/data/collections/Back to the Future Collection [boxset]/collection.xml"
[ ! -e /mnt/tb01/jellyfin/media/jellymesh-test ] || { echo "run down.sh first"; exit 1; }
while IFS= read -r file; do
  [ -e "$library/$file" ] || cp -p "$from/$file" "$library/$file"
done < "$kit/files.txt"
(cd "$library" && sha256sum -c --quiet "$kit/sha256.txt") && echo "all files back and verified"
for film in "Back To The Future (1985)" "Back To The Future Part II (1989)" "Back To The Future Part III (1990)"; do
  grep -q "$film" "$collection" || { cp -p "$kit/collection.xml" "$collection"; echo "collection file restored"; break; }
done
python3 - <<'PY'
import json,os,urllib.request
cfg=json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key=next(v for k,v in cfg.items() if "key" in k.lower() or "token" in k.lower())
urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8097/Library/Refresh",method="POST",headers={"X-Emby-Token":key}),timeout=60)
print("scan started")
PY
EOF
