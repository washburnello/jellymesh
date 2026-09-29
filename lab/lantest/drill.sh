#!/usr/bin/env bash
# M-3 drills on the running LAN test (up.sh), with walnut as the node under
# test and cedar as the group's owner.
#
#   drill.sh backup    back walnut up, replace it with a node restored from
#                      the backup into a fresh volume, check it is the same
#                      node and still serves cedar; then roll back to the
#                      original volume and check again
#   drill.sh start [volume]  run walnut on a volume (default: the original)
#   drill.sh recover   the compromise-recovery runbook: treat walnut's key
#                      as stolen, eject it, rebuild walnut with a new key,
#                      re-enrol it, check it serves again and that the old
#                      key is refused
#
# The backup passphrase is random, kept only in $state (0600), never printed.
set -euo pipefail
cedar=${CEDAR_HOST:-cedar}
walnut_ip=${WALNUT_IP:-192.168.87.20}
cedar_ip=${CEDAR_IP:-192.168.87.247}
state="$HOME/.config/jellymesh-lantest"
report() { printf '%-58s %s\n' "$1" "$2"; }
json() { local code=$1; shift; python3 -c "import sys,json; $code" "$@"; }
service_password=$(awk -F' / ' '/^service user:/{print $2}' "$state/credentials")

walnut_jm() { docker exec jm-lan-walnut /jellymesh "$@"; }
cedar_jm() { ssh "$cedar" docker exec jm-lan-cedar /jellymesh "$@"; }

start_walnut() { # volume
  docker rm -f jm-lan-walnut >/dev/null 2>&1 || true
  docker run -d --name jm-lan-walnut --restart unless-stopped --network jm-lan -v "$1:/data" \
    -p "$walnut_ip:18443:8443" \
    -e JELLYMESH_NODE_NAME=walnut -e JELLYMESH_PUBLIC_HOSTNAME="$walnut_ip:18443" \
    -e JELLYMESH_JELLYFIN_URL=http://jm-lan-jellyfin:8096 -e JELLYMESH_JELLYFIN_USER=jellymesh -e JELLYMESH_JELLYFIN_PASSWORD="$service_password" \
    jellymesh:lan >/dev/null
  for _ in $(seq 1 30); do walnut_jm status >/dev/null 2>&1 && return; sleep 1; done
  echo "walnut did not start"; docker logs --tail 20 jm-lan-walnut; exit 1
}

# cedar's Jellyfin streams a range of walnut's film from its relay; prints
# "exact" when the bytes match walnut's file.
bytes_check() {
  local offset=700000000 expected got
  expected=$(python3 -c "f=open('$HOME/Videos/Back to the Future/Back To The Future (1985) Remastered.mkv','rb');f.seek($offset);print(f.read(100).hex())")
  got=$(ssh "$cedar" "bash -s" <<EOF
python3 - <<'PY'
import json,os,urllib.request,urllib.parse
cfg=json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key=next(v for k,v in cfg.items() if "key" in k.lower() or "token" in k.lower())
def req(p,q={},h={}): return urllib.request.Request("http://127.0.0.1:8097"+p+"?"+urllib.parse.urlencode(q),headers={"X-Emby-Token":key,**h})
items=[i for i in json.load(urllib.request.urlopen(req("/Items",{"searchTerm":"Back to the Future","Recursive":"true","IncludeItemTypes":"Movie","Years":"1985","Fields":"Path"})))["Items"] if "jellymesh-test" in i.get("Path","")]
if not items: print("no-item"); raise SystemExit
try:
    r=urllib.request.urlopen(req("/Videos/%s/stream"%items[0]["Id"],{"static":"true"},{"Range":"bytes=$offset-%d"%($offset+99)}),timeout=60)
    print(r.read().hex())
except Exception as e: print("error-"+type(e).__name__)
PY
EOF
)
  [ "$got" = "$expected" ] && echo exact || echo "FAILED (${got:0:24})"
}

identity() { walnut_jm status | json 'd=json.load(sys.stdin); g=d.get("group") or {}; print(d["node_id"], d["fingerprint"][:16], g.get("id"), len(g.get("members") or []))'; }

backup_drill() {
  [ -f "$state/backup-passphrase" ] || (umask 077; openssl rand -hex 24 > "$state/backup-passphrase")
  local secret; secret=$(cat "$state/backup-passphrase")
  report "before: walnut node / fingerprint / group / members" "$(identity)"
  report "before: cedar streams walnut's bytes" "$(bytes_check)"
  # The image has no shell tools; backup refuses to overwrite, so each run
  # uses its own name, and a helper removes it from the volume afterwards.
  local name; name="drill-$(date +%s).jmb"
  docker exec -e JELLYMESH_BACKUP_PASSPHRASE="$secret" jm-lan-walnut /jellymesh backup "/data/$name"
  rm -f "$state/drill.jmb"
  (umask 077; docker cp "jm-lan-walnut:/data/$name" "$state/drill.jmb" >/dev/null)
  docker run --rm -v jm-lan-walnut-data:/data alpine:3 rm -f "/data/$name"
  report "backup taken" "$(stat -c %s "$state/drill.jmb") bytes, mode $(stat -c %a "$state/drill.jmb")"
  local before; before=$(identity)

  docker rm -f jm-lan-walnut >/dev/null
  docker volume rm jm-lan-walnut-restored >/dev/null 2>&1 || true
  docker volume create jm-lan-walnut-restored >/dev/null
  # The image runs as uid 65532, which cannot read an owner-only file. Hand
  # it a readable copy inside a private directory, removed straight after;
  # the backup is encrypted under the passphrase in any case.
  local hand; hand=$(mktemp -d "$state/restore.XXXXXX")
  cp "$state/drill.jmb" "$hand/drill.jmb" && chmod 0644 "$hand/drill.jmb"
  docker run --rm -v jm-lan-walnut-restored:/data -v "$hand/drill.jmb:/drill.jmb:ro" \
    -e JELLYMESH_BACKUP_PASSPHRASE="$secret" -e JELLYMESH_NODE_NAME=walnut jellymesh:lan restore /drill.jmb | head -1
  rm -rf "$hand"
  start_walnut jm-lan-walnut-restored
  walnut_jm sync >/dev/null 2>&1 || true
  local after; after=$(identity)
  report "restored: walnut node / fingerprint / group / members" "$after"
  report "restored node is the same node" "$([ "$before" = "$after" ] && echo yes || echo NO)"
  cedar_jm catalog-sync >/dev/null
  report "restored: cedar streams walnut's bytes" "$(bytes_check)"

  start_walnut jm-lan-walnut-data
  report "rolled back to the original volume: same node" "$([ "$(identity)" = "$before" ] && echo yes || echo NO)"
  report "rolled back: cedar streams walnut's bytes" "$(bytes_check)"
  docker volume rm jm-lan-walnut-restored >/dev/null
}

recover_drill() {
  local old_id old_fp
  old_id=$(walnut_jm status | json 'print(json.load(sys.stdin)["node_id"])')
  old_fp=$(walnut_jm status | json 'print(json.load(sys.stdin)["fingerprint"])')
  report "compromised node" "${old_id:0:12} (${old_fp:0:16})"
  # 1. Cut the key off: the owner ejects it.
  cedar_jm eject "$old_id" >/dev/null
  cedar_jm catalog-sync >/dev/null
  report "1. ejected; cedar's members" "$(cedar_jm status | json 'print(len(json.load(sys.stdin)["group"]["members"]))')"
  report "1. cedar can no longer play from the ejected key" "$([ "$(bytes_check)" = exact ] && echo "NO, it still plays" || echo yes)"
  # 3. Rebuild with a new identity; the old volume is kept for investigation.
  docker rm -f jm-lan-walnut >/dev/null
  docker volume rm jm-lan-walnut-rebuilt >/dev/null 2>&1 || true
  docker volume create jm-lan-walnut-rebuilt >/dev/null
  start_walnut jm-lan-walnut-rebuilt
  local new_fp; new_fp=$(walnut_jm status | json 'print(json.load(sys.stdin)["fingerprint"])')
  report "3. rebuilt with a new key" "${new_fp:0:16} (different: $([ "$new_fp" != "$old_fp" ] && echo yes || echo NO))"
  local library; library=$(walnut_jm libraries | json 'print([l.get("id") or l.get("Id") for l in json.load(sys.stdin) if (l.get("name") or l.get("Name"))=="Movies"][0])')
  walnut_jm publish "$library" -root /media/movies >/dev/null
  # 4. Re-enrol with a fresh invitation and approval.
  local code; code=$(cedar_jm invite -valid-for 1h | json 'print(json.load(sys.stdin)["short_code"])')
  walnut_jm join -address "$cedar_ip:18443" -code "$code" -wait 2m >"$state/rejoin.log" 2>&1 &
  local join=$! requests=""
  for _ in $(seq 1 20); do
    requests=$(cedar_jm requests)
    [ "$(echo "$requests" | json 'print(len(json.load(sys.stdin) or []))')" -ge 1 ] && break; sleep 1
  done
  local seen; seen=$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["fingerprint"])')
  report "4. the request shows the new fingerprint (confirm out of band)" "$([ "$seen" = "$new_fp" ] && echo yes || echo NO)"
  cedar_jm approve "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["inviter_id"])')" "$(echo "$requests" | json 'print(json.load(sys.stdin)[0]["invitation_id"])')" >/dev/null
  wait "$join" && report "4. rebuilt walnut rejoined" "yes"
  walnut_jm catalog-sync >/dev/null
  cedar_jm catalog-sync >/dev/null
  # cedar's Jellyfin holds each .strm's old reference until it next scans
  # (A-12); scan now, as its schedule would.
  ssh "$cedar" 'python3 - <<"PY"
import json,os,time,urllib.request
cfg=json.load(open(os.path.expanduser("~/.config/tfetch/config.json")))["jellyfin"]
key=next(v for k,v in cfg.items() if "key" in k.lower() or "token" in k.lower())
call=lambda m,p: urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:8097"+p,method=m,headers={"X-Emby-Token":key}),timeout=60).read()
call("POST","/Library/Refresh"); time.sleep(5)
while [t for t in json.loads(call("GET","/ScheduledTasks")) if t.get("Key")=="RefreshLibrary"][0]["State"]!="Idle": time.sleep(3)
PY'
  report "4. after cedar's scan, it streams the rebuilt walnut's bytes" "$(bytes_check)"
  # The stolen key, brought back, gets nowhere.
  docker rm -f jm-lan-walnut-old >/dev/null 2>&1 || true
  docker run -d --name jm-lan-walnut-old --network jm-lan -v jm-lan-walnut-data:/data \
    -e JELLYMESH_NODE_NAME=walnut -e JELLYMESH_PUBLIC_HOSTNAME="$walnut_ip:18444" jellymesh:lan >/dev/null
  sleep 5
  local old_sync; old_sync=$(docker exec jm-lan-walnut-old /jellymesh sync 2>&1 | tr -d ' \n')
  report "the old key's sync with the group (refused: Reached 0)" "$old_sync"
  docker rm -f jm-lan-walnut-old >/dev/null
  report "5. clean up: old volume kept for investigation" "jm-lan-walnut-data"
}

case "${1:-}" in
  start) start_walnut "${2:-jm-lan-walnut-data}" ;;
  backup) backup_drill ;;
  recover) recover_drill ;;
  *) echo "usage: drill.sh backup|recover"; exit 2 ;;
esac
