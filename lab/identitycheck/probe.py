"""Phase 4 identity probe against a throwaway Jellyfin (conformance M-10).

Run by run-throwaway.sh, which owns the container. Arguments: the base URL,
the media directory on the host, the same directory inside the container,
and a small real video clip for the local copies.
"""

import json
import os
import secrets
import shutil
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

base, host_media, container_media, clip = sys.argv[1:5]
header = 'MediaBrowser Client="lab", Device="lab", DeviceId="m10", Version="1"'


def call(method, path, token=None, body=None, query=None):
    url = base + path
    if query:
        url += "?" + urllib.parse.urlencode(query, doseq=True)
    auth = header + (f', Token="{token}"' if token else "")
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, method=method, headers={"Authorization": auth})
    if data is not None:
        request.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(request, timeout=60) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


def report(label, value):
    print(f"{label:<64} {value}")


def write(relative, content):
    path = os.path.join(host_media, relative)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as handle:
        handle.write(content)


def identifiers(ids):
    """Both NFO spellings, as the materializer writes them."""
    unique = "".join(f'<uniqueid type="{p}"{" default=\"true\"" if n == 0 else ""}>{v}</uniqueid>'
                     for n, (p, v) in enumerate(ids.items()))
    return unique + "".join(f"<{p}id>{v}</{p}id>" for p, v in ids.items())


def movie_nfo(title, year, provider, value=None):
    ids = provider if isinstance(provider, dict) else {provider: value}
    return (f'<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<movie><title>{title}</title>'
            f'<year>{year}</year>{identifiers(ids)}<lockdata>true</lockdata></movie>\n')


def show_nfo(title, tvdb):
    ids = tvdb if isinstance(tvdb, dict) else {"tvdb": tvdb}
    return (f'<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<tvshow><title>{title}</title>'
            f'{identifiers(ids)}<lockdata>true</lockdata></tvshow>\n')


def episode_nfo(title, season, episode):
    return (f'<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<episodedetails><title>{title}</title>'
            f'<season>{season}</season><episode>{episode}</episode><lockdata>true</lockdata></episodedetails>\n')


def generated_movie(title, year, jmid, source, provider, value=None):
    folder = f"movies/generated/Movies/{title} ({year}) [jmid-{jmid}]"
    write(f"{folder}/movie.nfo", movie_nfo(title, year, provider, value))
    write(f"{folder}/{title} ({year}) [jmid-{jmid}] - {source}.strm", f"http://127.0.0.1:9/r/{secrets.token_hex(8)}\n")
    return folder


# Each TV library gets its own identities, so state retained in one cannot
# turn up in the other.
prefix = {"tv": "8880", "tvg": "8881"}


def generated_episode(library, show, jmid, tvdb, season, episode, source, layout="plain"):
    tvdb = {p: prefix[library] + v for p, v in tvdb.items()} if isinstance(tvdb, dict) else prefix[library] + tvdb
    folder = f"{library}/generated/TV Shows/{show} [jmid-{jmid}]"
    write(f"{folder}/tvshow.nfo", show_nfo(show, tvdb))
    name = f"{show} S{season:02d}E{episode:02d}"
    if layout == "folder":  # a folder per episode, as movie versions use
        stem = f"{folder}/Season {season:02d}/{name}/{name} - {source}"
    elif layout == "bracket":
        stem = f"{folder}/Season {season:02d}/{name} [{source}]"
    else:
        stem = f"{folder}/Season {season:02d}/{name} - {source}"
    write(f"{stem}.nfo", episode_nfo(f"Episode {episode}", season, episode))
    write(f"{stem}.strm", f"http://127.0.0.1:9/r/{secrets.token_hex(8)}\n")
    return folder


def remove(relative):
    shutil.rmtree(os.path.join(host_media, relative))


# --- start-up ---------------------------------------------------------------
for _ in range(90):
    try:
        call("GET", "/Startup/Configuration")
        break
    except Exception:
        time.sleep(2)
admin_password, alice_password = secrets.token_hex(16), secrets.token_hex(16)
call("POST", "/Startup/Configuration", body={"UICulture": "en-US", "MetadataCountryCode": "US", "PreferredMetadataLanguage": "en"})
call("GET", "/Startup/User")
call("POST", "/Startup/User", body={"Name": "labadmin", "Password": admin_password})
call("POST", "/Startup/Complete")
admin = call("POST", "/Users/AuthenticateByName", body={"Username": "labadmin", "Pw": admin_password})["AccessToken"]
alice_id = call("POST", "/Users/New", admin, {"Name": "alice", "Password": alice_password})["Id"]
alice = call("POST", "/Users/AuthenticateByName", body={"Username": "alice", "Pw": alice_password})["AccessToken"]

no_fetchers = [{"Type": kind, "MetadataFetchers": [], "ImageFetchers": []} for kind in ("Movie", "Series", "Season", "Episode")]
for relative in ("movies/local", "movies/generated/Movies", "tv/local", "tv/generated/TV Shows",
                 "tvg/local", "tvg/generated/TV Shows"):
    os.makedirs(os.path.join(host_media, relative), exist_ok=True)

# The local copies, which are real files.
os.makedirs(os.path.join(host_media, "movies/local/Shared Film (2005)"))
shutil.copy(clip, os.path.join(host_media, "movies/local/Shared Film (2005)/Shared Film (2005).mkv"))
write("movies/local/Shared Film (2005)/movie.nfo", movie_nfo("Shared Film", 2005, "tmdb", "999104"))
for library in ("tv", "tvg"):
    write(f"{library}/local/Shared Show/tvshow.nfo", show_nfo("Shared Show", prefix[library] + "02"))
    write(f"{library}/local/Shared Show/Season 01/Shared Show S01E01.nfo", episode_nfo("Episode 1", 1, 1))
    shutil.copy(clip, os.path.join(host_media, f"{library}/local/Shared Show/Season 01/Shared Show S01E01.mkv"))

# The generated copies, which point nowhere: identity needs no playback.
generated_movie("Hist Film", 2004, "a1", "Cedar", "tmdb", "999101")
generated_movie("Resume Film", 2004, "a2", "Cedar", "tmdb", "999102")
generated_movie("Fav Film", 2004, "a3", "Cedar", "tmdb", "999103")
generated_movie("Shared Film", 2005, "a4", "Cedar", "tmdb", "999104")
generated_movie("Imdb Film", 2006, "a5", "Cedar", "imdb", "tt9990105")
for library in ("tv", "tvg"):
    for episode in (1, 2):
        generated_episode(library, "Hist Show", "s1", "01", 1, episode, "Cedar")
    for source in ("Cedar", "Walnut"):
        generated_episode(library, "Hist Show", "s1", "01", 1, 3, source)
        generated_episode(library, "Hist Show", "s1", "01", 1, 4, source, layout="folder")
        generated_episode(library, "Hist Show", "s1", "01", 1, 5, source, layout="bracket")
    generated_episode(library, "Shared Show", "s2", "02", 1, 1, "Cedar")
    generated_episode(library, "Cross Show", "s3", "03", 1, 1, "Cedar")
    generated_episode(library, "Cross Show", "s4", "03", 1, 2, "Walnut")


def create_library(name, collection, paths, grouping=False):
    options = {"EnableRealtimeMonitor": False, "MetadataSavers": [], "TypeOptions": no_fetchers,
               "EnableAutomaticSeriesGrouping": grouping}
    call("POST", "/Library/VirtualFolders", admin, {"LibraryOptions": options},
         {"name": name, "collectionType": collection, "paths": [f"{container_media}/{p}" for p in paths],
          "refreshLibrary": "false"})


create_library("Movies", "movies", ["movies/local", "movies/generated/Movies"])
create_library("TV", "tvshows", ["tv/local", "tv/generated/TV Shows"])
create_library("TVGrouped", "tvshows", ["tvg/local", "tvg/generated/TV Shows"], grouping=True)
libraries = {f["Name"]: f["ItemId"] for f in call("GET", "/Library/VirtualFolders", admin)}


def scan():
    started = time.time()
    call("POST", "/Library/Refresh", admin)
    time.sleep(3)
    for _ in range(200):
        tasks = call("GET", "/ScheduledTasks", admin)
        refresh = [t for t in tasks if t.get("Key") == "RefreshLibrary"][0]
        ended = (refresh.get("LastExecutionResult") or {}).get("EndTimeUtc", "")
        if refresh["State"] == "Idle" and ended:
            return f"{round(time.time() - started)} s, {refresh['LastExecutionResult'].get('Status')}"
        time.sleep(2)
    raise SystemExit("scan did not finish")


def items(library, kinds):
    return call("GET", "/Items", alice, query={
        "userId": alice_id, "ParentId": libraries[library], "Recursive": "true", "IncludeItemTypes": kinds,
        "Fields": "Path,ProviderIds,MediaSources,SeriesPresentationUniqueKey"})["Items"]


def find(library, kinds, predicate):
    return [item for item in items(library, kinds) if predicate(item)]


def generated(item):
    return "/generated/" in (item.get("Path") or "")


def state(item):
    data = item.get("UserData") or {}
    return (f"played={data.get('Played')} count={data.get('PlayCount')} "
            f"resume={data.get('PlaybackPositionTicks')} fav={data.get('IsFavorite')} key={data.get('Key')}")


def movie(title, want_generated=True):
    found = find("Movies", "Movie", lambda i: i["Name"] == title and generated(i) == want_generated)
    return found[0] if found else None


def episode(library, series, number, want_generated=True):
    found = find(library, "Episode", lambda i: i.get("SeriesName") == series and i.get("IndexNumber") == number
                 and generated(i) == want_generated)
    return found


print("== first scan:", scan(), "s")
for item in items("Movies", "Movie"):
    report(f"movie {item['Name']!r} generated={generated(item)}", f"providers={item.get('ProviderIds')}")
for library in ("TV", "TVGrouped"):
    for series in items(library, "Series"):
        report(f"{library} series {series['Name']!r} id={series['Id'][:8]}",
               f"path={series.get('Path', '').split('/media/')[-1]} providers={series.get('ProviderIds')}")
    for item in items(library, "Episode"):
        sources = [s.get("Name") for s in item.get("MediaSources") or []]
        report(f"{library} {item.get('SeriesName')} S{item.get('ParentIndexNumber')}E{item.get('IndexNumber')} "
               f"series={item.get('SeriesId', '')[:8]} gen={generated(item)}", f"versions={sources}")

for library in ("TV", "TVGrouped"):
    for series in items(library, "Series"):
        listed = call("GET", f"/Shows/{series['Id']}/Episodes", alice, query={"userId": alice_id, "Fields": "Path"})["Items"]
        report(f"{library} card {series['Name']!r} {series['Id'][:8]} lists",
               sorted(f"E{e.get('IndexNumber')}{'gen' if generated(e) else 'local'}" for e in listed))

print("== alice's actions")
call("POST", f"/UserPlayedItems/{movie('Hist Film')['Id']}", alice)
call("POST", f"/UserItems/{movie('Resume Film')['Id']}/UserData", alice, {"PlaybackPositionTicks": 3_000_000_000})
call("POST", f"/UserFavoriteItems/{movie('Fav Film')['Id']}", alice)
call("POST", f"/UserPlayedItems/{movie('Imdb Film')['Id']}", alice)
call("POST", f"/UserPlayedItems/{movie('Shared Film', want_generated=False)['Id']}", alice)
for library in ("TV", "TVGrouped"):
    call("POST", f"/UserPlayedItems/{episode(library, 'Hist Show', 1)[0]['Id']}", alice)
    call("POST", f"/UserItems/{episode(library, 'Hist Show', 2)[0]['Id']}/UserData", alice,
         {"PlaybackPositionTicks": 3_000_000_000})
    call("POST", f"/UserPlayedItems/{episode(library, 'Shared Show', 1, want_generated=False)[0]['Id']}", alice)
for title in ("Hist Film", "Resume Film", "Fav Film", "Imdb Film"):
    report(f"after marking: {title}", state(movie(title)))
report("local Shared Film", state(movie("Shared Film", want_generated=False)))
report("generated Shared Film, never touched", state(movie("Shared Film")))
for library in ("TV", "TVGrouped"):
    for number in (1, 2):
        report(f"{library} generated Hist Show E{number}", state(episode(library, "Hist Show", number)[0]))
    report(f"{library} local Shared Show E1", state(episode(library, "Shared Show", 1, want_generated=False)[0]))
    for item in episode(library, "Shared Show", 1):
        report(f"{library} generated Shared Show E1, never touched", state(item))

print("== purge: withdraw, scan, and return under another folder and source")
remove("movies/generated/Movies/Hist Film (2004) [jmid-a1]")
remove("movies/generated/Movies/Fav Film (2004) [jmid-a3]")
remove("movies/generated/Movies/Imdb Film (2006) [jmid-a5]")
for library in ("TV", "TVGrouped"):
    remove(f"{library.lower().replace('grouped', 'g')}/generated/TV Shows/Hist Show [jmid-s1]")
print("scan:", scan(), "s")
report("withdrawn movies still listed", [m["Name"] for m in items("Movies", "Movie")
                                          if m["Name"] in ("Hist Film", "Fav Film", "Imdb Film")])
report("withdrawn series still listed", [s["Name"] for lib in ("TV", "TVGrouped")
                                          for s in items(lib, "Series") if s["Name"] == "Hist Show"])
tasks = call("GET", "/ScheduledTasks", admin)
cleaners = [t for t in tasks if any(w in t["Name"] for w in ("Clean", "Optimi"))]
report("clean-up tasks run while withdrawn", [t["Name"] for t in cleaners])
for task in cleaners:
    call("POST", f"/ScheduledTasks/Running/{task['Id']}", admin)
time.sleep(20)
call("POST", "/System/Restart", admin)
time.sleep(10)
for _ in range(90):
    try:
        call("GET", "/System/Info", admin)
        break
    except Exception:
        time.sleep(2)
report("restarted while withdrawn", "yes")
generated_movie("Hist Film", 2004, "b1", "Walnut", "tmdb", "999101")
generated_movie("Fav Film", 2004, "b3", "Walnut", "tmdb", "999103")
generated_movie("Imdb Film", 2006, "b5", "Walnut", "imdb", "tt9990105")
for library in ("tv", "tvg"):
    for number in (1, 2):
        generated_episode(library, "Hist Show", "t1", "01", 1, number, "Walnut")
# A rename within one pass: the old folder goes and the new one arrives
# before Jellyfin scans, as when the lead source of a work changes.
remove("movies/generated/Movies/Resume Film (2004) [jmid-a2]")
generated_movie("Resume Film", 2004, "b2", "Walnut", "tmdb", "999102")
print("scan:", scan(), "s")
for title in ("Hist Film", "Resume Film", "Fav Film", "Imdb Film"):
    found = movie(title)
    report(f"returned: {title}", state(found) if found else "missing")
for library in ("TV", "TVGrouped"):
    for number in (1, 2):
        found = episode(library, "Hist Show", number)
        report(f"{library} returned Hist Show E{number}", state(found[0]) if found else "missing")


print("== when a work's identifiers change while it is withdrawn")
# Grow: known only by IMDb, it returns with TMDB as well. Shrink: the reverse.
generated_movie("Grow Film", 2007, "c1", "Cedar", "imdb", "tt9990106")
generated_movie("Shrink Film", 2007, "c2", "Cedar", {"tmdb": "999107", "imdb": "tt9990107"})
generated_movie("Swap Film", 2007, "c3", "Cedar", "tmdb", "999108")
for library in ("tv", "tvg"):
    generated_episode(library, "Grow Show", "u1", {"tmdb": "11"}, 1, 1, "Cedar")
    generated_episode(library, "Tvdb Show", "u2", {"tvdb": "12"}, 1, 1, "Cedar")
# Two generated copies of one work at once, both watched, both withdrawn in
# one scan: the case that aborts scans in jellyfin/jellyfin#16975.
generated_movie("Twin Film", 2008, "c4", "Cedar", "tmdb", "999109")
generated_movie("Twin Film", 2008, "c5", "Walnut", "tmdb", "999109")
print("scan:", scan())
call("POST", f"/UserPlayedItems/{movie('Grow Film')['Id']}", alice)
call("POST", f"/UserPlayedItems/{movie('Shrink Film')['Id']}", alice)
call("POST", f"/UserPlayedItems/{movie('Swap Film')['Id']}", alice)
for twin in find("Movies", "Movie", lambda i: i["Name"] == "Twin Film"):
    call("POST", f"/UserPlayedItems/{twin['Id']}", alice)
for library in ("TV", "TVGrouped"):
    for show in ("Grow Show", "Tvdb Show"):
        call("POST", f"/UserPlayedItems/{episode(library, show, 1)[0]['Id']}", alice)
twins = find("Movies", "Movie", lambda i: i["Name"] == "Twin Film")
report("twin copies listed", len(twins))
merge = urllib.request.Request(base + "/Videos/MergeVersions?" + urllib.parse.urlencode({"ids": ",".join(t["Id"] for t in twins)}),
                               method="POST", headers={"Authorization": header + f', Token="{alice}"'})
try:
    with urllib.request.urlopen(merge, timeout=30) as response:
        report("non-administrator POST /Videos/MergeVersions", response.status)
except urllib.error.HTTPError as error:
    report("non-administrator POST /Videos/MergeVersions", error.code)
for folder in ("Grow Film (2007) [jmid-c1]", "Shrink Film (2007) [jmid-c2]", "Swap Film (2007) [jmid-c3]",
               "Twin Film (2008) [jmid-c4]", "Twin Film (2008) [jmid-c5]"):
    remove(f"movies/generated/Movies/{folder}")
for library in ("tv", "tvg"):
    remove(f"{library}/generated/TV Shows/Grow Show [jmid-u1]")
    remove(f"{library}/generated/TV Shows/Tvdb Show [jmid-u2]")
print("withdrawal scan:", scan())
generated_movie("Grow Film", 2007, "d1", "Walnut", {"tmdb": "999106", "imdb": "tt9990106"})
generated_movie("Shrink Film", 2007, "d2", "Walnut", "imdb", "tt9990107")
generated_movie("Swap Film", 2007, "d3", "Walnut", "imdb", "tt9990108")  # a different identifier only
generated_movie("Twin Film", 2008, "d4", "Walnut", "tmdb", "999109")
for library in ("tv", "tvg"):
    generated_episode(library, "Grow Show", "v1", {"tvdb": "11", "tmdb": "11"}, 1, 1, "Walnut")
    generated_episode(library, "Tvdb Show", "v2", {"tmdb": "12", "tvdb": "12"}, 1, 1, "Walnut")
print("return scan:", scan())
for title in ("Grow Film", "Shrink Film", "Swap Film", "Twin Film"):
    found = movie(title)
    report(f"returned: {title}", state(found) if found else "missing")
for library in ("TV", "TVGrouped"):
    for show in ("Grow Show", "Tvdb Show"):
        found = episode(library, show, 1)
        report(f"{library} returned {show} E1", state(found[0]) if found else "missing")
