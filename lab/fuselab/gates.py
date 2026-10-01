"""The spike's gates (#60), rerun against the real build (#73). Each
subcommand runs one gate against the lab env.sh made and prints what it
measured. The harness follows lab/fusespike/gates.py; what stood in for
Jellymesh there is the real chain here:

    source Jellyfin -> fault proxy -> source node -> destination node's
    read service -> jellymesh mount -> destination Jellyfin

    gates.py g1|g2|g3|g6|g7 ...
"""
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "fusespike"))
from jf import Jellyfin  # noqa: E402

BASE = os.environ.get("FUSELAB_BASE", os.path.expanduser("~/.local/share/jellymesh-fuselab"))
URL = "http://127.0.0.1:18230"
PROXY = "http://127.0.0.1:18310"
SECRETS = json.load(open(os.path.join(BASE, "state", "secrets.json")))
jf = Jellyfin(URL, SECRETS["destination"]["token"])
source_jf = Jellyfin("http://127.0.0.1:18220", SECRETS["source"]["token"])
LIBS = ("Jellymesh Movies",)
FILMS = os.environ.get("FILMS", os.path.expanduser("~/Videos/Back to the Future"))


def report(label, value):
    print(f"{label:<62} {value}", flush=True)


def get_json(url):
    return json.loads(urllib.request.urlopen(url, timeout=10).read())


def docker(*args):
    return subprocess.run(["docker", *args], capture_output=True, text=True)


def source_stats(reset=False):
    return get_json(PROXY + "/stats" + ("?reset=1" if reset else ""))


def node_status(name="fl-dst"):
    out = docker("exec", name, "/jellymesh", "status").stdout
    return json.loads(out) if out else {}


def faults(mode="normal", latency_ms=0, rate_kbps=0):
    body = json.dumps({"mode": mode, "latency_ms": latency_ms, "rate_kbps": rate_kbps}).encode()
    urllib.request.urlopen(urllib.request.Request(PROXY + "/control", data=body, method="POST"), timeout=10).read()


def mb(n):
    return f"{n / 1e6:.1f} MB"


def media_info(library):
    items = jf.items(library, "Path,MediaSources,MediaStreams")
    probed = sum(1 for i in items if any(ms.get("MediaStreams") for ms in i.get("MediaSources", [])))
    runtime = sum(1 for i in items if i.get("RunTimeTicks"))
    return len(items), probed, runtime


def settle(seconds=15, timeout=900):
    """Waits until no media bytes have crossed the source for `seconds`."""
    started, last, quiet = time.time(), None, 0
    while time.time() - started < timeout:
        now = source_stats()["bytes"]
        quiet = quiet + 1 if now == last else 0
        last = now
        if quiet >= seconds:
            return
        time.sleep(1)


def stuck_threads():
    """Counts Jellyfin container threads in uninterruptible sleep (D)."""
    out = subprocess.run(["docker", "exec", "fl-dst-jf", "sh", "-c", "cat /proc/[0-9]*/task/*/stat 2>/dev/null"],
                         capture_output=True, text=True, timeout=30).stdout
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


def item_named(name, library="Jellymesh Movies"):
    return [i for i in jf.items(library) if i["Name"] == name][0]["Id"]


def fuse_item(n):
    return item_named(f"Spike Film {n:02d}")


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


def recovered(n, timeout=90):
    """G5: a fresh read of an uncached part of a film works again without manual steps."""
    began = time.time()
    while time.time() - began < timeout:
        try:
            _, _, got = timed_read(fuse_item(n), 50_000_000 + int(time.time() * 7) % 900_000_000, 2 << 20)
            if got == 2 << 20:
                return round(time.time() - began, 1)
        except Exception:
            pass
        time.sleep(2)
    return None


def mount_restarts():
    return docker("inspect", "fl-mount", "--format", "{{.RestartCount}}").stdout.strip()


def wait_mount(present=True, timeout=60):
    path = os.path.join(BASE, "share", "films", "Movies")
    for _ in range(timeout * 2):
        try:
            if os.path.isdir(path) == present:
                return True
        except OSError:
            pass
        time.sleep(0.5)
    return False


def wait_node(name="fl-dst", timeout=60):
    for _ in range(timeout):
        if docker("exec", name, "/jellymesh", "status").returncode == 0:
            return True
        time.sleep(1)
    return False


def signal_mount(name):
    """Signals the mount process from a helper sharing its PID namespace, as
    a crash or deadlock from inside would: `docker kill --signal` marks the
    container as stopped by hand, and its restart policy then never acts."""
    return docker("run", "--rm", "--pid", "container:fl-mount", "alpine:3", "kill", f"-{name}", "1")


def inject(mode):
    if mode in ("refuse", "hang"):
        faults(mode)
    elif mode == "crawl":
        faults("slow", rate_kbps=1)
    elif mode == "reader-stopped":
        docker("stop", "-t", "1", "fl-dst")
    elif mode == "mount-crashed":
        signal_mount("USR1")
    elif mode == "mount-deadlocked":
        signal_mount("USR2")
    elif mode == "mount-frozen":
        docker("pause", "fl-mount")


def restore(mode):
    faults("normal")
    if mode == "reader-stopped":
        docker("start", "fl-dst")
        wait_node()
    if mode == "mount-frozen":
        docker("unpause", "fl-mount")


def g1(*modes):
    print("== G1: never hangs; G5: recovers by itself")
    modes = modes or ("refuse", "hang", "crawl", "reader-stopped", "mount-crashed", "mount-deadlocked", "mount-frozen")
    for index, mode in enumerate(modes):
        monitor = Monitor(); monitor.start()
        streamer = Streamer(fuse_item(index + 1), 200_000_000); streamer.start()
        time.sleep(5)
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
        report(f"{mode}: G5 recovery (mount restarts so far {mount_restarts()})", f"{recovered(index + 10)} s")


def g3():
    print("== G3: no surprise transfers")
    source_stats(reset=True)
    seconds, status = jf.scan()
    settle(10)
    report("scan with nothing new", f"{seconds} s, {status}; source bytes {mb(source_stats(reset=True)['bytes'])}")

    tasks = [t for t in jf.call("GET", "/ScheduledTasks", {"isHidden": "false"}) if t.get("Key") != "RefreshLibrary"]
    for task in tasks:
        jf.call("POST", f"/ScheduledTasks/Running/{task['Id']}")
    time.sleep(5)
    for _ in range(600):
        if not [t["Name"] for t in jf.call("GET", "/ScheduledTasks") if t["State"] != "Idle"]:
            break
        time.sleep(2)
    settle(10)
    report(f"every scheduled task ({len(tasks)}), extraction off", mb(source_stats(reset=True)["bytes"]))

    library = jf.library_id("Jellymesh Movies")
    jf.call("POST", f"/Items/{library}/Refresh",
            {"Recursive": "true", "MetadataRefreshMode": "FullRefresh", "ImageRefreshMode": "FullRefresh", "ReplaceAllMetadata": "true"})
    time.sleep(10)
    settle(15)
    report("full metadata refresh", mb(source_stats(reset=True)["bytes"]))

    tree = os.path.join(BASE, "share", "films")
    film = next(os.path.join(root, f) for root, _, files in os.walk(os.path.join(tree, "Movies")) for f in files if f.endswith(".mkv"))
    for label, command in {"find": ["find", tree, "-type", "f"], "du -sh": ["du", "-sh", tree], "cat a film": ["cat", film],
                           "grep -r (reads every file)": ["grep", "-r", "-c", "zzzz", tree], "md5sum a film": ["md5sum", film]}.items():
        result = subprocess.run(command, capture_output=True, text=True, timeout=120)
        tail = (result.stderr.strip().splitlines() or [""])[-1][-60:]
        report(f"  walnut user: {label}", f"exit {result.returncode} {tail}")
    other = subprocess.run(["docker", "run", "--rm", "--user", "1234:1234", "--mount",
                            f"type=bind,src={os.path.join(BASE, 'share')},dst=/s,readonly,bind-propagation=rslave",
                            "alpine:3", "sh", "-c", "head -c 100 \"$(find /s/films/Movies -name '*.mkv' | head -1)\" >/dev/null; echo exit=$?"],
                           capture_output=True, text=True, timeout=120)
    report("  another container as uid 1234: read a film", (other.stdout + other.stderr).strip()[-80:])
    settle(5)
    report("bytes pulled by all non-Jellyfin readers", mb(source_stats(reset=True)["bytes"]))


def g3_extraction(minutes="3"):
    """The pacing guard (#69): trickplay extraction left on by mistake."""
    print("== G3b: trickplay extraction on, with pacing")
    library = jf.library_id("Jellymesh Movies")
    folder = [l for l in jf.call("GET", "/Library/VirtualFolders") if l["ItemId"] == library][0]
    options = dict(folder["LibraryOptions"], EnableTrickplayImageExtraction=True)
    jf.call("POST", "/Library/VirtualFolders/LibraryOptions", body={"Id": library, "LibraryOptions": options})
    paced_before = node_status()["presentation"]["reads"]["paced_seconds"]
    source_stats(reset=True)
    task = [t for t in jf.call("GET", "/ScheduledTasks") if t.get("Key") == "RefreshTrickplayImages"][0]
    jf.call("POST", f"/ScheduledTasks/Running/{task['Id']}")
    for minute in range(int(minutes)):
        time.sleep(60)
        stats = source_stats(reset=True)
        top = sorted(stats["items"].values(), reverse=True)[:3]
        report(f"  minute {minute + 1}: source bytes (top films)", f"{mb(stats['bytes'])} ({', '.join(mb(b) for b in top)})")
    jf.call("DELETE", f"/ScheduledTasks/Running/{task['Id']}")
    options["EnableTrickplayImageExtraction"] = False
    jf.call("POST", "/Library/VirtualFolders/LibraryOptions", body={"Id": library, "LibraryOptions": options})
    report("  reads paced (s)", round(node_status()["presentation"]["reads"]["paced_seconds"] - paced_before, 1))
    settle(10)


def g6():
    print("== G6: feels normal")
    for latency in (0, 40):
        faults("normal", latency_ms=latency)
        print(f"-- source latency {latency} ms per request")
        # A fresh daemon each round, so its cache does not flatter the reads.
        docker("restart", "-t", "2", "fl-dst")
        wait_node()
        item = item_named("Back to the Future")
        began = time.time()
        info = jf.call("POST", f"/Items/{item}/PlaybackInfo", body={})
        elapsed = round(time.time() - began, 3)
        size = info["MediaSources"][0].get("Size") or 0
        first, done, _ = timed_read(item, 0, 1 << 20)
        seek_first, seek_done, _ = timed_read(item, 700_000_000, 1 << 20)
        # Within the film's burst (ten minutes of play), then past it, where
        # a read as fast as possible is paced (#69).
        _, sustained, got = timed_read(item, 100_000_000, 100 << 20)
        _, paced, paced_got = timed_read(item, 300_000_000, 80 << 20)
        report("  PlaybackInfo", f"{elapsed} s (reported size {size})")
        report("  start (first byte / 1 MB)", f"{first} / {done} s")
        report("  seek to 700 MB (first byte / 1 MB)", f"{seek_first} / {seek_done} s")
        report("  sustained 100 MB, inside the burst", f"{got / sustained / 1e6:.0f} MB/s")
        report("  a further 80 MB as fast as possible, past it (paced)", f"{paced_got / paced / 1e6:.1f} MB/s")
    faults("normal")


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


def add_films(n, tag, sync=True):
    """Adds n films at the source: hard links under new titles and
    identities, a source scan, and a catalog pass on both nodes, so the
    destination materializes them."""
    real = sorted(f for f in os.listdir(FILMS) if f.endswith(".mkv"))
    base = 980000 + abs(hash(tag)) % 9000 * 10
    for index in range(n):
        title, year = f"Added {tag} {index:02d}", 2031
        folder = os.path.join(BASE, "media", "Movies", f"{title} ({year})")
        os.makedirs(folder, exist_ok=True)
        target = os.path.join(folder, f"{title} ({year}).mkv")
        if not os.path.exists(target):
            os.link(os.path.join(FILMS, real[index % len(real)]), target)
        with open(os.path.join(folder, "movie.nfo"), "w") as handle:
            handle.write(f'<?xml version="1.0"?><movie><title>{title}</title><year>{year}</year>'
                         f'<uniqueid type="tmdb" default="true">{base + index}</uniqueid><tmdbid>{base + index}</tmdbid>'
                         f'<lockdata>true</lockdata></movie>')
    source_jf.scan()
    if sync:
        docker("exec", "fl-src", "/jellymesh", "catalog-sync")
        docker("exec", "fl-dst", "/jellymesh", "catalog-sync")


def probed_named(prefix):
    return sum(1 for i in jf.items("Jellymesh Movies", "MediaSources,MediaStreams") if i["Name"].startswith(prefix)
               and any(ms.get("MediaStreams") for ms in i.get("MediaSources", [])))


def wait_scan_idle(timeout=900):
    for _ in range(timeout):
        if [t for t in jf.call("GET", "/ScheduledTasks") if t.get("Key") == "RefreshLibrary"][0]["State"] == "Idle":
            return
        time.sleep(1)


def g2():
    print("== G2: the library never shrinks")
    me = jf.call("GET", "/Users/Me")["Id"]
    for item in jf.items("Jellymesh Movies")[:5]:
        jf.call("POST", f"/UserPlayedItems/{item['Id']}", {"userId": me})
    jf.scan()
    all_ok = True

    before = snapshot()
    docker("stop", "-t", "5", "fl-mount")
    wait_mount(present=False)
    jf.scan()
    all_ok &= compare("A. scan with the mount cleanly stopped", before, snapshot())
    docker("start", "fl-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("A. after it returns and a rescan", before, snapshot())

    docker("update", "--restart", "no", "fl-mount")
    signal_mount("USR1")
    time.sleep(3)
    jf.scan()
    all_ok &= compare("B. scan with the mount crashed and down", before, snapshot())
    docker("update", "--restart", "unless-stopped", "fl-mount")
    docker("start", "fl-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("B. after it returns and a rescan", before, snapshot())

    add_films(12, "c")
    time.sleep(70)  # the mount's cache time, so the new films are listed
    faults("normal", latency_ms=300)  # slow the probes so the scan is still running
    jf.call("POST", "/Library/Refresh")
    time.sleep(4)
    signal_mount("USR1")
    report("C. mount crashed mid-scan", "restarted by policy")
    wait_scan_idle()
    faults("normal")
    wait_mount()
    after_c = snapshot()
    new_c = len(after_c[0]["Jellymesh Movies"]) - len(before[0]["Jellymesh Movies"])
    all_ok &= compare("C. after the interrupted scan", before, after_c, expect_new=new_c)
    report("C. new films present after the interrupted scan", f"{new_c} of 12")
    jf.scan()
    all_ok &= compare("C. after a clean rescan", before, snapshot(), expect_new=12)
    report("C. new films with media info", f"{probed_named('Added c')} of 12")
    if probed_named("Added c") < 12:
        time.sleep(30)  # healing: retried every 10 s, then marked changed
        jf.scan()
        report("C. ... after healing and another rescan", f"{probed_named('Added c')} of 12")

    before = snapshot()
    for name in ("fl-dst-jf", "fl-mount", "fl-dst"):
        docker("stop", "-t", "5", name)
    docker("start", "fl-dst-jf")
    time.sleep(20)
    for _ in range(90):
        try:
            jf.call("GET", "/System/Info"); break
        except Exception:
            time.sleep(1)
    jf.scan()
    all_ok &= compare("D. reboot: Jellyfin up and scanned before the mount", before, snapshot())
    docker("start", "fl-dst"); wait_node(); docker("start", "fl-mount"); wait_mount()
    jf.scan()
    all_ok &= compare("D. after the mount comes up and a rescan", before, snapshot())

    add_films(6, "e")
    time.sleep(70)
    docker("stop", "-t", "1", "fl-dst")
    jf.scan()
    after_e = snapshot()
    all_ok &= compare("E. scan of new films with the node down", before, after_e, expect_new=6)
    report("E. those films with media info while it is down", f"{probed_named('Added e')} of 6")
    docker("start", "fl-dst"); wait_node()
    time.sleep(35)  # the mount resends its failures; the node retries every 10 s
    jf.scan()
    report("E. ... after the node returns and a rescan", f"{probed_named('Added e')} of 6")
    first, done, got = timed_read(item_named("Added e 00"), 0, 1 << 20)
    report("E. one of them plays", f"{got} bytes in {done} s")
    all_ok &= probed_named("Added e") == 6
    print("G2", "PASS" if all_ok else "FAIL")


def container_memory():
    out = docker("stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}").stdout
    usage = {}
    for line in out.splitlines():
        name, used = line.split(" ", 1)
        if name.startswith("fl-"):
            usage[name.removeprefix("fl-")] = used.split(" / ")[0]
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
        for t in [t for t in streams if t.ended is not None]:
            streams.remove(t)
        while len(streams) < 2:
            t = Streamer(fuse_item(random.randint(1, 30)), random.randint(0, 1_000_000_000)); t.start()
            streams.append(t)

    while datetime.datetime.now() < end:
        cycle += 1
        keep_streaming()
        forced = os.environ.get("FORCE_EVENTS", "").split(",")
        event = forced[cycle - 1] if cycle <= len(forced) and forced[0] else random.choices(events, weights)[0]
        monitor.worst, restart_expected = 0.0, event in ("jellyfin-restart", "add-films")
        problems = []
        began = time.time()
        try:
            if event == "add-films":
                added += 1
                add_films(3, f"s{added:03d}")
                time.sleep(65)  # the mount's cache time
                jf.scan()
                new_ids, _ = snapshot()
                grown = len(new_ids["Jellymesh Movies"]) - len(expected["Jellymesh Movies"])
                if grown != 3:
                    problems.append(f"expected 3 new films, got {grown}")
                expected = new_ids
            elif event == "scan":
                jf.scan()
            elif event == "jellyfin-restart":
                docker("restart", "-t", "10", "fl-dst-jf")
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
        say(f"cycle {cycle:4d} {event:<17} {time.time() - began:5.0f} s  recovery {recovery} s  ping worst {monitor.worst:.2f} s  "
            f"items {'/'.join(str(len(expected[l])) for l in LIBS)}  mount restarts {mount_restarts()}  mem {container_memory()}  "
            + ("OK" if not problems else "FAIL: " + "; ".join(problems)))
        time.sleep(random.randint(10, 40))
    monitor.running = False
    say(f"soak end: {cycle} cycles, {failures} with problems")


if __name__ == "__main__":
    globals()[sys.argv[1]](*sys.argv[2:])
