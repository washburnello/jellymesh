"""Sets up the gate lab's Jellyfins (#73). Passwords are generated, kept in
<base>/credentials (mode 0600), and never printed.

    setup.py source <url> <base>       admin, a Movies library on /media/movies, and
                                       a non-administrator service user that sees it
    setup.py destination <url> <base>  admin and a service user, no libraries yet
    setup.py libraries <url> <base>    the remote-films library on the mount, with
                                       extraction, savers, and fetchers off
    setup.py service <base> <side>     prints a side's service password, for the node
"""
import json
import os
import secrets
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "fusespike"))
from jf import Jellyfin, wait_ready  # noqa: E402

OPTIONS = {"EnableRealtimeMonitor": False, "EnableTrickplayImageExtraction": False, "ExtractTrickplayImagesDuringLibraryScan": False,
           "EnableChapterImageExtraction": False, "ExtractChapterImagesDuringLibraryScan": False, "SaveLocalMetadata": False,
           "MetadataSavers": [], "EnableInternetProviders": False,
           "TypeOptions": [{"Type": "Movie", "MetadataFetchers": [], "ImageFetchers": []}]}


def secrets_file(base):
    path = os.path.join(base, "state", "secrets.json")
    return json.load(open(path)) if os.path.exists(path) else {}


def keep(base, side, **values):
    current = secrets_file(base)
    current.setdefault(side, {}).update(values)
    os.makedirs(os.path.join(base, "state"), exist_ok=True)
    path = os.path.join(base, "state", "secrets.json")
    old = os.umask(0o077)
    json.dump(current, open(path, "w"))
    os.umask(old)
    os.chmod(path, 0o600)


def first_run(url, base, side, admin):
    wait_ready(url)
    password = secrets.token_hex(12)
    anonymous = Jellyfin(url, None)
    anonymous.call("POST", "/Startup/Configuration", body={"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"})
    anonymous.call("GET", "/Startup/User")
    anonymous.call("POST", "/Startup/User", body={"Name": admin, "Password": password})
    anonymous.call("POST", "/Startup/Complete")
    jf = Jellyfin.login(url, admin, password)
    keep(base, side, admin=admin, admin_password=password, token=jf.token)
    return jf


def service_user(jf, base, side, libraries):
    password = secrets.token_hex(16)
    user = jf.call("POST", "/Users/New", body={"Name": "jellymesh", "Password": password})["Id"]
    policy = jf.call("GET", f"/Users/{user}")["Policy"]
    policy.update({"IsAdministrator": False, "EnableAllFolders": False, "EnabledFolders": libraries})
    jf.call("POST", f"/Users/{user}/Policy", body=policy)
    keep(base, side, service_password=password)


def source(url, base):
    jf = first_run(url, base, "source", "admin")
    options = dict(OPTIONS, EnableInternetProviders=False)
    jf.call("POST", "/Library/VirtualFolders", {"name": "Movies", "collectionType": "movies", "paths": ["/media/movies"], "refreshLibrary": "true"},
            {"LibraryOptions": options})
    library = jf.library_id("Movies")
    service_user(jf, base, "source", [library])
    expected = len([d for d in os.listdir(os.path.join(base, "media", "Movies"))])
    for _ in range(120):
        items = jf.items("Movies", "ProviderIds,MediaSources")
        sized = [i for i in items if i.get("MediaSources") and i["MediaSources"][0].get("Size")]
        if len(sized) >= expected:
            break
        time.sleep(3)
    print(f"source: {len(items)} films, {len(sized)} sized, of {expected}")


def destination(url, base):
    jf = first_run(url, base, "destination", "fuselab")
    service_user(jf, base, "destination", [])
    old = os.umask(0o077)
    with open(os.path.join(base, "credentials"), "w") as handle:
        handle.write(f"gate lab destination Jellyfin: http://192.168.87.20:18230 (LAN) or {url}\n"
                     f"user: fuselab\npassword: {secrets_file(base)['destination']['admin_password']}\n")
    os.umask(old)
    print("destination: set up; credentials in", os.path.join(base, "credentials"))


def libraries(url, base):
    name = os.environ.get("LIB_NAME", "Jellymesh Movies")
    path = os.environ.get("LIB_PATH", "/remote/films/Movies")
    jf = Jellyfin(url, secrets_file(base)["destination"]["token"])
    if not any(l["Name"] == name for l in jf.call("GET", "/Library/VirtualFolders")):
        jf.call("POST", "/Library/VirtualFolders", {"name": name, "collectionType": "movies",
                                                    "paths": [path], "refreshLibrary": "false"}, {"LibraryOptions": OPTIONS})
    seconds, status = jf.scan()
    print(f"destination: scanned in {seconds} s ({status}); {len(jf.items(name))} films in {name}")


if __name__ == "__main__":
    command = sys.argv[1]
    if command == "service":
        print(secrets_file(sys.argv[2])[sys.argv[3]]["service_password"])
    else:
        {"source": source, "destination": destination, "libraries": libraries}[command](sys.argv[2], sys.argv[3])
