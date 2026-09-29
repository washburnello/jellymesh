"""A small Jellyfin API client for the spike's scripts."""
import json
import time
import urllib.error
import urllib.parse
import urllib.request

HEADER = 'MediaBrowser Client="fusespike", Device="fusespike", DeviceId="fusespike", Version="1"'


def wait_ready(url, seconds=180):
    for _ in range(seconds):
        try:
            urllib.request.urlopen(url + "/System/Info/Public", timeout=3).read()
            urllib.request.urlopen(url + "/Startup/Configuration", timeout=3).read()
            return
        except urllib.error.HTTPError as error:
            if error.code == 401:
                return  # set up already
            time.sleep(1)
        except Exception:
            time.sleep(1)
    raise SystemExit("jellyfin did not start")


class Jellyfin:
    def __init__(self, url, token):
        self.url, self.token = url, token

    @classmethod
    def login(cls, url, user, password):
        anonymous = cls(url, None)
        token = anonymous.call("POST", "/Users/AuthenticateByName", body={"Username": user, "Pw": password})["AccessToken"]
        return cls(url, token)

    def request(self, method, path, query=None, body=None, headers=None, timeout=60):
        url = self.url + path + ("?" + urllib.parse.urlencode(query, doseq=True) if query else "")
        auth = HEADER + (f', Token="{self.token}"' if self.token else "")
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(url, data=data, method=method, headers={"Authorization": auth, **(headers or {})})
        if data is not None:
            request.add_header("Content-Type", "application/json")
        return urllib.request.urlopen(request, timeout=timeout)

    def call(self, method, path, query=None, body=None, timeout=60):
        with self.request(method, path, query, body, timeout=timeout) as response:
            raw = response.read()
            return json.loads(raw) if raw else None

    def library_id(self, name):
        return [l["ItemId"] for l in self.call("GET", "/Library/VirtualFolders") if l["Name"] == name][0]

    def items(self, library, fields="Path,MediaSources,ProviderIds"):
        return self.call("GET", "/Items", {"ParentId": self.library_id(library), "Recursive": "true",
                                           "IncludeItemTypes": "Movie", "Fields": fields})["Items"]

    def scan(self, timeout=3600):
        """Starts a library scan and waits for it to finish; returns (seconds, status)."""
        started = time.time()
        self.call("POST", "/Library/Refresh")
        time.sleep(3)
        while time.time() - started < timeout:
            task = [t for t in self.call("GET", "/ScheduledTasks") if t.get("Key") == "RefreshLibrary"][0]
            if task["State"] == "Idle":
                return round(time.time() - started, 1), (task.get("LastExecutionResult") or {}).get("Status")
            time.sleep(2)
        raise TimeoutError("scan did not finish")
