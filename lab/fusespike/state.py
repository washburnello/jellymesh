"""Generates the spike's local state: the mount's manifest, the reader's
sources, and the .strm baseline library. Three real films plus synthetic
ones reusing their bytes, each with its own reference so that no two items
share a cache entry and probe costs are counted honestly.

    state.py <films-dir> <base>
"""
import json
import os
import sys

films, base = sys.argv[1], sys.argv[2]
real = [
    ("Back to the Future", 1985, "105", "tt0088763", "Back To The Future (1985) Remastered.mkv"),
    ("Back to the Future Part II", 1989, "165", "tt0096874", "Back To The Future Part II (1989) Remastered.mkv"),
    ("Back to the Future Part III", 1990, "196", "tt0099088", "Back To The Future Part III (1990) Remastered.mkv"),
]
works = list(real)
for n in range(30):
    title, year, _, _, file = real[n % 3]
    works.append((f"Spike Film {n + 1:02d}", 2000 + n, str(900100 + n), f"tt99{n:05d}", file))


def nfo(title, year, tmdb, imdb, lock):
    return (f'<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<movie><title>{title}</title><year>{year}</year>'
            f'<uniqueid type="tmdb" default="true">{tmdb}</uniqueid><tmdbid>{tmdb}</tmdbid>'
            f'<uniqueid type="imdb">{imdb}</uniqueid><imdbid>{imdb}</imdbid>'
            f'<lockdata>{"true" if lock else "false"}</lockdata></movie>\n')


manifest, sources = [], {}
mtime = 1759000000
for root, lock in (("Movies", True), ("MoviesNoLock", False)):
    for index, (title, year, tmdb, imdb, file) in enumerate(works):
        size = os.path.getsize(os.path.join(films, file))
        folder = f"{root}/{title} ({year}) [jmid-{root[-4:].lower()}{index:03d}]"
        ref = f"{root.lower()}-{index:03d}"
        manifest.append({"path": f"{folder}/{os.path.basename(folder)} - walnut.mkv", "size": size, "mtime": mtime, "ref": ref})
        manifest.append({"path": f"{folder}/movie.nfo", "content": nfo(title, year, tmdb, imdb, lock), "mtime": mtime})
        sources[ref] = {"url": "http://fusespike-source:8300/files/" + file.replace(" ", "%20"), "size": size}

strm_root = os.path.join(base, "strm", "Movies")
for index, (title, year, tmdb, imdb, file) in enumerate(works):
    folder = os.path.join(strm_root, f"{title} ({year}) [jmid-strm{index:03d}]")
    os.makedirs(folder, exist_ok=True)
    with open(os.path.join(folder, f"{os.path.basename(folder)} - walnut.strm"), "w") as handle:
        handle.write("http://fusespike-source:8300/files/" + file.replace(" ", "%20") + "\n")
    with open(os.path.join(folder, "movie.nfo"), "w") as handle:
        handle.write(nfo(title, year, tmdb, imdb, True))

json.dump({"files": manifest}, open(os.path.join(base, "state", "manifest.json"), "w"))
json.dump(sources, open(os.path.join(base, "state", "sources.json"), "w"))
print(f"state: {len(works)} works x 2 FUSE roots, {len(works)} .strm")
