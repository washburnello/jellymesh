"""Sets up the spike's Jellyfin: an admin account (password kept in
<base>/credentials, mode 0600, never printed) and three movie libraries, all
with extraction, savers, and online fetchers off:

  FUSE Movies        /remote/mnt/Movies        NFOs with lockdata
  FUSE Movies NoLock /remote/mnt/MoviesNoLock  NFOs without lockdata
  STRM Movies        /strm/Movies              the .strm baseline

    jellyfin_setup.py <url> <base>
"""
import json
import os
import secrets
import sys
import time

sys.path.insert(0, os.path.dirname(__file__))
from jf import Jellyfin, wait_ready

url, base = sys.argv[1], sys.argv[2]
wait_ready(url)
password = secrets.token_hex(12)
anonymous = Jellyfin(url, None)
anonymous.call("POST", "/Startup/Configuration", body={"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"})
anonymous.call("GET", "/Startup/User")
anonymous.call("POST", "/Startup/User", body={"Name": "spike", "Password": password})
anonymous.call("POST", "/Startup/Complete")
old = os.umask(0o077)
with open(os.path.join(base, "credentials"), "w") as handle:
    handle.write(f"FUSE spike Jellyfin: http://192.168.87.249:18130 (LAN) or {url}\nuser: spike\npassword: {password}\n")
os.umask(old)
jf = Jellyfin.login(url, "spike", password)
json.dump({"token": jf.token}, open(os.path.join(base, "state", "token.json"), "w"))
os.chmod(os.path.join(base, "state", "token.json"), 0o600)

types = [{"Type": t, "MetadataFetchers": [], "ImageFetchers": []} for t in ("Movie",)]
options = {"EnableRealtimeMonitor": False, "EnableTrickplayImageExtraction": False, "ExtractTrickplayImagesDuringLibraryScan": False,
           "EnableChapterImageExtraction": False, "ExtractChapterImagesDuringLibraryScan": False, "SaveLocalMetadata": False,
           "MetadataSavers": [], "EnableInternetProviders": False, "TypeOptions": types}
for name, path in (("FUSE Movies", "/remote/mnt/Movies"), ("FUSE Movies NoLock", "/remote/mnt/MoviesNoLock"), ("STRM Movies", "/strm/Movies")):
    jf.call("POST", "/Library/VirtualFolders", {"name": name, "collectionType": "movies", "paths": [path], "refreshLibrary": "false"},
            {"LibraryOptions": options})
print("jellyfin set up; libraries:", [l["Name"] for l in jf.call("GET", "/Library/VirtualFolders")])
