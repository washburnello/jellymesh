"""M-14 (#72): a node moved from .strm to FUSE presentation keeps its users'
state. Runs against a second lab instance started in strm mode:

    LAB=fm NET=fusemig SRC_PORT=18240 DST_PORT=18250 PROXY_PORT=18330 \\
      FUSELAB_BASE=~/.local/share/jellymesh-fusemig SYNTHETIC=0 PRESENTATION=strm ./env.sh up
    LAB=fm ... python3 migrate.py

It plays a film through the .strm library, gives three films a watched state,
a resume position, and a favourite, switches the node to FUSE as
docs/operator-fuse.md says, and checks the new items carry the same state.
"""
import json
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "fusespike"))
from jf import Jellyfin  # noqa: E402

LAB = os.environ.get("LAB", "fm")
BASE = os.path.expanduser(os.environ.get("FUSELAB_BASE", "~/.local/share/jellymesh-fusemig"))
URL = f"http://127.0.0.1:{os.environ.get('DST_PORT', '18250')}"
jf = Jellyfin(URL, json.load(open(os.path.join(BASE, "state", "secrets.json")))["destination"]["token"])
OLD, NEW = "Jellymesh Movies (strm)", "Jellymesh Movies"
here = os.path.dirname(os.path.abspath(__file__))


def report(label, value):
    print(f"{label:<62} {value}", flush=True)


def env(*args, presentation):
    return subprocess.run([os.path.join(here, "env.sh"), *args], capture_output=True, text=True,
                          env=dict(os.environ, PRESENTATION=presentation, FUSELAB_BASE=BASE))


def by_name(library):
    return {i["Name"]: i for i in jf.items(library, "Path,MediaSources,ProviderIds")}


def state(item_id, me):
    data = jf.call("GET", f"/UserItems/{item_id}/UserData", {"userId": me})
    return {"played": data.get("Played"), "position": data.get("PlaybackPositionTicks"), "favourite": data.get("IsFavorite")}


def read(item_id):
    response = jf.request("GET", f"/Videos/{item_id}/stream", {"static": "true"}, headers={"Range": "bytes=0-1048575"})
    got = len(response.read())
    response.close()
    return got


me = jf.call("GET", "/Users/Me")["Id"]
old = by_name(OLD)
report("before: films in the .strm library", len(old))
report("before: a film plays through the .strm", f"{read(old['Back to the Future']['Id'])} bytes")
wanted = {"Back to the Future": {"Played": True},
          "Back to the Future Part II": {"PlaybackPositionTicks": 30 * 60 * 10**7},
          "Back to the Future Part III": {"IsFavorite": True}}
for name, data in wanted.items():
    jf.call("POST", f"/UserItems/{old[name]['Id']}/UserData", {"userId": me}, body=data)
before = {name: state(old[name]["Id"], me) for name in wanted}
report("before: state set", json.dumps(before))

# The switch, as the operator guide says: the node in fuse mode, the mount,
# a catalog pass, a new library on the mount, and a scan.
subprocess.run(["docker", "rm", "-f", f"{LAB}-dst"], capture_output=True)
env("start_dst", presentation="fuse")
env("start_mount", presentation="fuse")
for _ in range(30):
    if subprocess.run(["docker", "exec", f"{LAB}-dst", "/jellymesh", "status"], capture_output=True).returncode == 0:
        break
    time.sleep(1)
synced = env("sync", presentation="fuse").stdout
report("switch: catalog pass", synced.strip()[-120:])
for _ in range(60):
    if os.path.isdir(os.path.join(BASE, "share", "films", "Movies")):
        break
    time.sleep(1)
report("switch: .strm files left", sum(1 for _, _, files in os.walk(os.path.join(BASE, "generated")) for f in files if f.endswith(".strm")))
jf.call("POST", "/Library/VirtualFolders", {"name": NEW, "collectionType": "movies", "paths": ["/remote/films/Movies"], "refreshLibrary": "false"},
        {"LibraryOptions": {"EnableRealtimeMonitor": False, "EnableTrickplayImageExtraction": False, "EnableChapterImageExtraction": False,
                            "SaveLocalMetadata": False, "MetadataSavers": [], "EnableInternetProviders": False}})
jf.scan()
new = by_name(NEW)
report("after: films in the .strm library", len(by_name(OLD)))
report("after: films in the FUSE library", len(new))
after = {name: state(new[name]["Id"], me) for name in wanted if name in new}
report("after: state on the new items", json.dumps(after))
ok = after == before
report("after: a film plays through the mount", f"{read(new['Back to the Future']['Id'])} bytes")

# The operator then removes the empty .strm library.
old_id = [l for l in jf.call("GET", "/Library/VirtualFolders") if l["Name"] == OLD][0]
jf.call("DELETE", "/Library/VirtualFolders", {"name": OLD, "refreshLibrary": "true"})
jf.scan()
again = {name: state(new[name]["Id"], me) for name in wanted if name in new}
report("after removing the .strm library and a scan", json.dumps(again))
ok = ok and again == before
print("M-14", "PASS" if ok else "FAIL")
