"""Gate checks for the FUSE spike (#60). Each subcommand runs one gate
against the environment env.sh made and prints what it measured.

    gates.py g3|g6|g1|g2|g5 ...
"""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request

sys.path.insert(0, os.path.dirname(__file__))
from jf import Jellyfin

BASE = os.environ.get("FUSESPIKE_BASE", os.path.expanduser("~/.local/share/jellymesh-fusespike"))
URL = "http://127.0.0.1:18130"
jf = Jellyfin(URL, json.load(open(os.path.join(BASE, "state", "token.json")))["token"])
LIBS = ("FUSE Movies", "FUSE Movies NoLock", "STRM Movies")


def report(label, value):
    print(f"{label:<62} {value}", flush=True)


def get_json(url):
    return json.loads(urllib.request.urlopen(url, timeout=10).read())


def stats(reset=False):
    q = "?reset=1" if reset else ""
    return get_json("http://127.0.0.1:18300/stats" + q), get_json("http://127.0.0.1:18200/stats" + q)


def faults(mode="normal", latency_ms=0, rate_kbps=0):
    body = json.dumps({"mode": mode, "latency_ms": latency_ms, "rate_kbps": rate_kbps}).encode()
    urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:18300/control", data=body, method="POST"), timeout=10).read()


def split(source, reader):
    refs = reader["refs"]
    lock = sum(v for k, v in refs.items() if k.startswith("movies-"))
    nolock = sum(v for k, v in refs.items() if k.startswith("moviesnolock-"))
    return {"fuse_lock": lock, "fuse_nolock": nolock, "strm_direct": source["bytes"] - reader["fetched"], "total": source["bytes"]}


def mb(n):
    return f"{n / 1e6:.1f} MB"


def media_info(library):
    items = jf.items(library, "Path,MediaSources,MediaStreams")
    probed = sum(1 for i in items if any(ms.get("MediaStreams") for ms in i.get("MediaSources", [])))
    runtime = sum(1 for i in items if i.get("RunTimeTicks"))
    return len(items), probed, runtime


def settle(seconds=15, timeout=900):
    """Waits until no bytes have crossed the source for `seconds`."""
    started, last, quiet = time.time(), None, 0
    while time.time() - started < timeout:
        now = get_json("http://127.0.0.1:18300/stats")["bytes"]
        quiet = quiet + 1 if now == last else 0
        last = now
        if quiet >= seconds:
            return
        time.sleep(1)


def g3():
    print("== G3: no surprise transfers")
    stats(reset=True)
    seconds, status = jf.scan()
    settle()
    moved = split(*stats(reset=True))
    report("first scan", f"{seconds} s, {status}")
    for library in LIBS:
        count, probed, runtime = media_info(library)
        report(f"  {library}: items / probed / with runtime", f"{count} / {probed} / {runtime}")
    report("  bytes: FUSE lockdata / FUSE no lockdata / STRM", f"{mb(moved['fuse_lock'])} / {mb(moved['fuse_nolock'])} / {mb(moved['strm_direct'])}")
    for key, name in (("fuse_lock", "FUSE Movies"), ("fuse_nolock", "FUSE Movies NoLock")):
        count = media_info(name)[0]
        report(f"  per item, {name}", mb(moved[key] / max(count, 1)))

    seconds, status = jf.scan()
    settle(10)
    report("second scan (nothing changed)", f"{seconds} s, {status}; bytes {mb(stats(reset=True)[0]['bytes'])}")

    tasks = [t for t in jf.call("GET", "/ScheduledTasks", {"isHidden": "false"}) if t.get("Key") != "RefreshLibrary"]
    for task in tasks:
        jf.call("POST", f"/ScheduledTasks/Running/{task['Id']}")
    time.sleep(5)
    for _ in range(600):
        running = [t["Name"] for t in jf.call("GET", "/ScheduledTasks") if t["State"] != "Idle"]
        if not running:
            break
        time.sleep(2)
    settle(10)
    moved = split(*stats(reset=True))
    report(f"every scheduled task ({len(tasks)})", f"FUSE {mb(moved['fuse_lock'] + moved['fuse_nolock'])}, STRM {mb(moved['strm_direct'])}")

    for library in ("FUSE Movies", "FUSE Movies NoLock"):
        jf.call("POST", f"/Items/{jf.library_id(library)}/Refresh",
                {"Recursive": "true", "MetadataRefreshMode": "FullRefresh", "ImageRefreshMode": "FullRefresh", "ReplaceAllMetadata": "true"})
    time.sleep(10)
    settle(15)
    moved = split(*stats(reset=True))
    report("full metadata refresh, both FUSE libraries", f"lockdata {mb(moved['fuse_lock'])}, no lockdata {mb(moved['fuse_nolock'])}")

    tree = os.path.join(BASE, "share", "mnt")
    film = next(os.path.join(root, f) for root, _, files in os.walk(os.path.join(tree, "Movies")) for f in files if f.endswith(".mkv"))
    commands = {
        "find": ["find", tree, "-type", "f"],
        "du -sh": ["du", "-sh", tree],
        "cat a film": ["cat", film],
        "grep -r (reads every file)": ["grep", "-r", "-c", "zzzz", tree],
        "md5sum a film": ["md5sum", film],
    }
    for label, command in commands.items():
        result = subprocess.run(command, capture_output=True, text=True, timeout=120)
        tail = (result.stderr.strip().splitlines() or [""])[-1][-60:]
        report(f"  walnut user: {label}", f"exit {result.returncode} {tail}")
    other = subprocess.run(["docker", "run", "--rm", "--user", "1234:1234", "--mount",
                            f"type=bind,src={os.path.join(BASE, 'share')},dst=/s,readonly,bind-propagation=rslave",
                            "alpine:3", "sh", "-c", "head -c 100 \"$(find /s/mnt/Movies -name '*.mkv' | head -1)\" >/dev/null; echo exit=$?"],
                           capture_output=True, text=True, timeout=120)
    report("  another container as uid 1234: read a film", (other.stdout + other.stderr).strip()[-80:])
    settle(5)
    report("bytes pulled by all non-Jellyfin readers", mb(stats(reset=True)[0]["bytes"]))



def timed_read(item_id, start=0, length=1 << 20, limit_bytes=None):
    """Reads through Jellyfin's direct stream; returns (first byte s, done s, bytes)."""
    headers = {"Range": f"bytes={start}-{start + length - 1}"}
    began = time.time()
    response = jf.request("GET", f"/Videos/{item_id}/stream", {"static": "true"}, headers=headers, timeout=120)
    first = None
    total = 0
    while True:
        chunk = response.read(64 << 10)
        if first is None:
            first = time.time() - began
        if not chunk:
            break
        total += len(chunk)
        if limit_bytes and total >= limit_bytes:
            break
    response.close()
    return round(first, 3), round(time.time() - began, 3), total


def pick(library, title="Back to the Future"):
    return [i for i in jf.items(library) if i["Name"] == title][0]


def g6():
    print("== G6: feels normal")
    for latency in (0, 40):
        faults("normal", latency_ms=latency)
        print(f"-- source latency {latency} ms per request")
        for library in ("FUSE Movies", "STRM Movies"):
            item = pick(library)
            # A fresh reference each round, so caches do not flatter FUSE.
            subprocess.run(["docker", "restart", "-t", "2", "fusespike-reader"], capture_output=True)
            time.sleep(2)
            began = time.time()
            jf.call("POST", f"/Items/{item['Id']}/PlaybackInfo", body={})
            info = round(time.time() - began, 3)
            size = [ms for ms in jf.call("POST", f"/Items/{item['Id']}/PlaybackInfo", body={})["MediaSources"]][0].get("Size") or 0
            first, done, _ = timed_read(item["Id"], 0, 1 << 20)
            seek_at = 700_000_000
            seek_first, seek_done, _ = timed_read(item["Id"], seek_at, 1 << 20)
            _, sustained, got = timed_read(item["Id"], 100_000_000, 200 << 20)
            report(f"  {library}: PlaybackInfo", f"{info} s (reported size {size})")
            report(f"  {library}: start (first byte / 1 MB)", f"{first} / {done} s")
            report(f"  {library}: seek to 700 MB (first byte / 1 MB)", f"{seek_first} / {seek_done} s")
            report(f"  {library}: sustained 200 MB", f"{got / sustained / 1e6:.0f} MB/s")
    faults("normal")



def stuck_threads():
    """Counts Jellyfin container threads in uninterruptible sleep (D)."""
    out = subprocess.run(["docker", "exec", "fusespike-jellyfin", "sh", "-c",
                          "cat /proc/[0-9]*/task/*/stat 2>/dev/null"], capture_output=True, text=True, timeout=30).stdout
    states = [line.rsplit(")", 1)[1].split()[0] for line in out.splitlines() if ")" in line]
    return states.count("D"), len(states)


class Monitor(threading.Thread):
    """Pings Jellyfin twice a second and keeps the worst response time."""
    def __init__(self):
        super().__init__(daemon=True)
        self.worst, self.failures, self.running = 0.0, 0, True

    def run(self):
        while self.running:
            began = time.time()
            try:
                urllib.request.urlopen(URL + "/System/Ping", timeout=30).read()
            except Exception:
                self.failures += 1
            self.worst = max(self.worst, time.time() - began)
            time.sleep(0.5)


class Streamer(threading.Thread):
    """Streams a film through Jellyfin at playback pace and notes how it ends."""
    def __init__(self, item_id, offset):
        super().__init__(daemon=True)
        self.item_id, self.offset = item_id, offset
        self.bytes, self.ended, self.error, self.last_byte = 0, None, None, time.time()

    def run(self):
        began = time.time()
        try:
            response = jf.request("GET", f"/Videos/{self.item_id}/stream", {"static": "true"},
                                  headers={"Range": f"bytes={self.offset}-"}, timeout=120)
            while True:
                chunk = response.read(256 << 10)
                if not chunk:
                    break
                self.bytes += len(chunk)
                self.last_byte = time.time()
                time.sleep(0.05)  # about 5 MB/s, a high film bitrate
        except Exception as error:
            self.error = type(error).__name__
        self.ended = time.time() - began


def fuse_item(n):
    return [i for i in jf.items("FUSE Movies") if i["Name"] == f"Spike Film {n:02d}"][0]["Id"]


def recovered(n, timeout=90):
    """G5: a fresh read of an uncached film works again without manual steps."""
    began = time.time()
    while time.time() - began < timeout:
        try:
            _, _, got = timed_read(fuse_item(n), 50_000_000 + n * 7_000_000, 2 << 20)
            if got == 2 << 20:
                return round(time.time() - began, 1)
        except Exception:
            pass
        time.sleep(2)
    return None


def inject(mode):
    if mode in ("refuse", "hang"):
        faults(mode)
    elif mode == "crawl":
        faults("slow", rate_kbps=1)
    elif mode == "reader-stopped":
        subprocess.run(["docker", "stop", "-t", "1", "fusespike-reader"], capture_output=True)
    elif mode == "mount-crashed":
        subprocess.run(["docker", "exec", "fusespike-mount", "kill", "-USR1", "1"], capture_output=True)
    elif mode == "mount-deadlocked":
        subprocess.run(["docker", "exec", "fusespike-mount", "kill", "-USR2", "1"], capture_output=True)
    elif mode == "mount-frozen":
        subprocess.run(["docker", "pause", "fusespike-mount"], capture_output=True)


def restore(mode):
    faults("normal")
    if mode == "reader-stopped":
        subprocess.run(["docker", "start", "fusespike-reader"], capture_output=True)
    if mode == "mount-frozen":
        subprocess.run(["docker", "unpause", "fusespike-mount"], capture_output=True)


def g1(*modes):
    print("== G1: never hangs; G5: recovers by itself")
    modes = modes or ("refuse", "hang", "crawl", "reader-stopped", "mount-crashed", "mount-deadlocked", "mount-frozen")
    for index, mode in enumerate(modes):
        subprocess.run(["docker", "restart", "-t", "1", "fusespike-reader"], capture_output=True)
        time.sleep(2)
        monitor = Monitor(); monitor.start()
        streamer = Streamer(fuse_item(index + 1), 200_000_000); streamer.start()
        time.sleep(5)
        began = time.time()
        inject(mode)
        streamer.join(timeout=45)
        worst_stuck = 0
        for _ in range(3):
            worst_stuck = max(worst_stuck, stuck_threads()[0])
            time.sleep(1)
        restore(mode)
        monitor.running = False; monitor.join()
        ended = f"ended after {streamer.ended - 5:.1f} s ({streamer.error or 'eof'})" if streamer.ended else "STILL RUNNING after 45 s"
        report(f"{mode}: stream", ended)
        report(f"{mode}: Jellyfin ping worst / failures", f"{monitor.worst:.2f} s / {monitor.failures}")
        report(f"{mode}: threads stuck in D", worst_stuck)
        restarts = subprocess.run(["docker", "inspect", "fusespike-mount", "--format", "{{.RestartCount}}"], capture_output=True, text=True).stdout.strip()
        report(f"{mode}: G5 recovery (mount restarts so far {restarts})", f"{recovered(index + 10)} s")



def snapshot():
    """Item IDs per library, and the watched items of the admin user."""
    me = jf.call("GET", "/Users/Me")["Id"]
    ids, watched = {}, set()
    for library in LIBS:
        items = jf.call("GET", "/Items", {"ParentId": jf.library_id(library), "Recursive": "true", "IncludeItemTypes": "Movie", "userId": me})["Items"]
        ids[library] = {i["Id"] for i in items}
        watched |= {i["Id"] for i in items if (i.get("UserData") or {}).get("Played")}
    return ids, watched


def compare(label, before, after, expect_new=0):
    (ids_before, watched_before), (ids_after, watched_after) = before, after
    lost = sum(len(ids_before[l] - ids_after[l]) for l in LIBS)
    gained = sum(len(ids_after[l] - ids_before[l]) for l in LIBS)
    counts = "/".join(str(len(ids_after[l])) for l in LIBS)
    ok = lost == 0 and gained == expect_new and watched_before <= watched_after
    report(f"{label}", f"{'PASS' if ok else 'FAIL'}: items {counts}, lost {lost}, new {gained}, watched kept {len(watched_before & watched_after)}/{len(watched_before)}")
    return ok


def docker(*args):
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def wait_mount(present=True, timeout=60):
    path = os.path.join(BASE, "share", "mnt", "Movies")
    for _ in range(timeout * 2):
        try:
            if os.path.isdir(path) == present:
                return True
        except OSError:
            pass
        time.sleep(0.5)
    return False


def add_films(n, tag):
    """Adds n new FUSE films (new refs) to the state; the mount and reader
    pick them up when they next start."""
    state = os.path.join(BASE, "state")
    manifest = json.load(open(os.path.join(state, "manifest.json")))
    sources = json.load(open(os.path.join(state, "sources.json")))
    template = next(e for e in manifest["files"] if e.get("ref") == "movies-000")
    for index in range(n):
        ref = f"movies-{tag}{index:02d}"
        folder = f"Movies/Added {tag} {index:02d} (2031) [jmid-{tag}{index:02d}]"
        manifest["files"].append({"path": f"{folder}/{os.path.basename(folder)} - walnut.mkv", "size": template["size"], "mtime": 1759000000, "ref": ref})
        manifest["files"].append({"path": f"{folder}/movie.nfo", "mtime": 1759000000,
                                  "content": f"<?xml version=\"1.0\"?><movie><title>Added {tag} {index:02d}</title><year>2031</year><tmdbid>98{index:04d}</tmdbid><lockdata>true</lockdata></movie>"})
        sources[ref] = sources["movies-000"]
    json.dump(manifest, open(os.path.join(state, "manifest.json"), "w"))
    json.dump(sources, open(os.path.join(state, "sources.json"), "w"))


def g2():
    print("== G2: the library never shrinks")
    me = jf.call("GET", "/Users/Me")["Id"]
    for item in jf.items("FUSE Movies")[:5] + jf.items("STRM Movies")[:2]:
        jf.call("POST", f"/UserPlayedItems/{item['Id']}", {"userId": me})
    jf.scan()
    all_ok = True

    before = snapshot()
    docker("stop", "-t", "5", "fusespike-mount")
    wait_mount(present=False)
    jf.scan()
    all_ok &= compare("A. scan with the mount cleanly stopped", before, snapshot())
    docker("start", "fusespike-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("A. after it returns and a rescan", before, snapshot())

    docker("update", "--restart", "no", "fusespike-mount")
    docker("exec", "fusespike-mount", "kill", "-USR1", "1")
    time.sleep(3)
    jf.scan()
    all_ok &= compare("B. scan with the mount crashed and down", before, snapshot())
    docker("update", "--restart", "unless-stopped", "fusespike-mount")
    docker("start", "fusespike-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("B. after it returns and a rescan", before, snapshot())

    add_films(12, "c")
    docker("restart", "-t", "1", "fusespike-reader")
    docker("exec", "fusespike-mount", "kill", "-USR1", "1")  # restarts with the new manifest
    wait_mount()
    time.sleep(2)
    faults("normal", latency_ms=300)  # slow the probes so the scan is still running
    jf.call("POST", "/Library/Refresh")
    time.sleep(4)
    docker("exec", "fusespike-mount", "kill", "-USR1", "1")
    report("C. mount crashed mid-scan", "restarted by policy")
    for _ in range(600):
        if [t for t in jf.call("GET", "/ScheduledTasks") if t.get("Key") == "RefreshLibrary"][0]["State"] == "Idle":
            break
        time.sleep(1)
    faults("normal")
    after_c = snapshot()
    all_ok &= compare("C. after the interrupted scan", before, after_c, expect_new=len(after_c[0]["FUSE Movies"]) - len(before[0]["FUSE Movies"]))
    report("C. new films present after the interrupted scan", f"{len(after_c[0]['FUSE Movies']) - len(before[0]['FUSE Movies'])} of 12")
    jf.scan()
    after_c2 = snapshot()
    all_ok &= compare("C. after a clean rescan", before, after_c2, expect_new=12)
    probed = sum(1 for i in jf.items("FUSE Movies", "MediaSources,MediaStreams") if i["Name"].startswith("Added c")
                 and any(ms.get("MediaStreams") for ms in i.get("MediaSources", [])))
    report("C. new films with media info", f"{probed} of 12")

    before = snapshot()
    for name in ("fusespike-jellyfin", "fusespike-mount", "fusespike-reader"):
        docker("stop", "-t", "5", name)
    docker("start", "fusespike-jellyfin")
    time.sleep(20)
    for _ in range(90):
        try:
            jf.call("GET", "/System/Info"); break
        except Exception:
            time.sleep(1)
    jf.scan()
    all_ok &= compare("D. reboot: Jellyfin up and scanned before the mount", before, snapshot())
    docker("start", "fusespike-reader"); docker("start", "fusespike-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("D. after the mount comes up and a rescan", before, snapshot())

    add_films(6, "e")
    docker("exec", "fusespike-mount", "kill", "-USR1", "1"); wait_mount(); time.sleep(2)
    docker("stop", "-t", "1", "fusespike-reader")
    jf.scan()
    after_e = snapshot()
    all_ok &= compare("E. scan of new films with the reader down", before, after_e, expect_new=6)
    docker("start", "fusespike-reader"); time.sleep(3)
    jf.scan()
    probed = sum(1 for i in jf.items("FUSE Movies", "MediaSources,MediaStreams") if i["Name"].startswith("Added e")
                 and any(ms.get("MediaStreams") for ms in i.get("MediaSources", [])))
    report("E. those films with media info after the reader returns and a rescan", f"{probed} of 6")
    first, done, got = timed_read([i for i in jf.items("FUSE Movies") if i["Name"] == "Added e 00"][0]["Id"], 0, 1 << 20)
    report("E. one of them plays", f"{got} bytes in {done} s")
    print("G2", "PASS" if all_ok else "FAIL")



def container_memory():
    out = docker("stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}").stdout
    usage = {}
    for line in out.splitlines():
        name, used = line.split(" ", 1)
        if name.startswith("fusespike-"):
            usage[name.removeprefix("fusespike-")] = used.split(" / ")[0]
    return usage


def g7(until="07:30", log_path=None):
    """The soak: continuous playback, a random disruption every cycle, and
    every invariant checked after each one, until the given local time."""
    import datetime
    import random
    hour, minute = map(int, until.split(":"))
    now = datetime.datetime.now()
    end = now.replace(hour=hour, minute=minute, second=0, microsecond=0)
    if end <= now:
        end += datetime.timedelta(days=1)
    log = open(log_path or os.path.join(BASE, "soak.log"), "a", buffering=1)

    def say(line):
        stamped = f"{datetime.datetime.now():%H:%M:%S} {line}"
        print(stamped, flush=True)
        log.write(stamped + "\n")

    events = ["refuse", "hang", "crawl", "reader-stopped", "mount-crashed", "mount-deadlocked",
              "mount-frozen", "add-films", "scan", "jellyfin-restart"]
    weights = [2, 2, 1, 2, 3, 2, 2, 1, 2, 1]
    expected, watched = snapshot()
    monitor = Monitor(); monitor.start()
    streams, cycle, failures, added = [], 0, 0, 0
    say(f"soak start; until {end:%Y-%m-%d %H:%M}; items {'/'.join(str(len(expected[l])) for l in LIBS)}; watched {len(watched)}")

    def keep_streaming():
        while len(streams) < 2 or any(t.ended is not None for t in streams):
            for t in [t for t in streams if t.ended is not None]:
                streams.remove(t)
            if len(streams) < 2:
                t = Streamer(fuse_item(random.randint(1, 30)), random.randint(0, 1_000_000_000)); t.start()
                streams.append(t)

    while datetime.datetime.now() < end:
        cycle += 1
        keep_streaming()
        forced = os.environ.get("FORCE_EVENTS", "").split(",")
        event = forced[cycle - 1] if cycle <= len(forced) and forced[0] else random.choices(events, weights)[0]
        monitor.worst, restart_expected = 0.0, event == "jellyfin-restart"
        problems = []
        began = time.time()
        try:
            if event == "add-films":
                added += 1
                add_films(3, f"s{added:03d}")
                docker("restart", "-t", "1", "fusespike-reader")
                # Wait for the mount to have restarted, not merely for its
                # folder to exist: the old mount is still up for a moment,
                # and scanning it misses the new films (soak cycle 41).
                restarts = docker("inspect", "fusespike-mount", "--format", "{{.RestartCount}}").stdout.strip()
                docker("exec", "fusespike-mount", "kill", "-USR1", "1")
                for _ in range(60):
                    if docker("inspect", "fusespike-mount", "--format", "{{.RestartCount}}").stdout.strip() != restarts:
                        break
                    time.sleep(0.5)
                wait_mount(); time.sleep(3)
                jf.scan()
                for library in LIBS:
                    pass
                new_ids, _ = snapshot()
                grown = len(new_ids["FUSE Movies"]) - len(expected["FUSE Movies"])
                if grown != 3:
                    problems.append(f"expected 3 new films, got {grown}")
                expected = new_ids
            elif event == "scan":
                jf.scan()
            elif event == "jellyfin-restart":
                docker("restart", "-t", "10", "fusespike-jellyfin")
                for _ in range(120):
                    try:
                        jf.call("GET", "/System/Info"); break
                    except Exception:
                        time.sleep(1)
            else:
                inject(event)
                time.sleep(random.randint(20, 50))
                restore(event)
        except Exception as error:
            problems.append(f"event error {type(error).__name__}: {error}")
        time.sleep(5)
        # Invariants.
        try:
            recovery = recovered(random.randint(1, 30), timeout=120)
            if recovery is None:
                problems.append("no recovery within 120 s")
            stuck, _ = stuck_threads()
            if stuck:
                time.sleep(25)
                stuck, _ = stuck_threads()
                if stuck:
                    problems.append(f"{stuck} threads stuck in D after 30 s")
            if monitor.worst > 3 and not restart_expected:
                problems.append(f"Jellyfin ping took {monitor.worst:.1f} s")
            ids, now_watched = snapshot()
            lost = sum(len(expected[l] - ids[l]) for l in LIBS)
            if lost:
                problems.append(f"{lost} items lost")
            if not watched <= now_watched:
                problems.append(f"{len(watched - now_watched)} watched states lost")
            expected = {l: expected[l] | ids[l] for l in LIBS}
        except Exception as error:
            problems.append(f"check error {type(error).__name__}: {error}")
            recovery = None
        failures += bool(problems)
        restarts = docker("inspect", "fusespike-mount", "--format", "{{.RestartCount}}").stdout.strip()
        say(f"cycle {cycle:4d} {event:<17} {time.time() - began:5.0f} s  recovery {recovery} s  ping worst {monitor.worst:.2f} s  "
            f"items {'/'.join(str(len(expected[l])) for l in LIBS)}  mount restarts {restarts}  mem {container_memory()}  "
            + ("OK" if not problems else "FAIL: " + "; ".join(problems)))
        time.sleep(random.randint(10, 40))
    monitor.running = False
    for t in streams:
        t.ended = t.ended or 0
    say(f"soak end: {cycle} cycles, {failures} with problems")


if __name__ == "__main__":
    globals()[sys.argv[1]](*sys.argv[2:])
