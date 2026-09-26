# Browser-driven client test (M-1a)

Work in progress. Drives Jellyfin Web with Playwright so the Web half of the
client matrix can be exercised without a person clicking.

## Status

- `probe.mjs` — logs in successfully against the lab instance. Working.
- `play.mjs` — opens the item, selects a version, attempts playback and a seek.
  **Not yet working:** the play button is found but reported "not visible", so
  the selector does not match what Jellyfin Web 10.11.11 actually renders. The
  remaining work is selector discovery against the real DOM, not a design
  problem.

## Requirements

Playwright is installed but its bundled Chromium build does not match the
version it expects. These scripts therefore point at the system browser:

```
executablePath: '/usr/bin/chromium'
```

A distro Chromium carries H.264, which Playwright's bundled build does not, so
this exercises real decode rather than forcing a transcode.

## Run

```bash
export PATH="$HOME/.local/share/mise/installs/node/26.7.0/bin:$PATH"
node lab/browser-test/probe.mjs
node lab/browser-test/play.mjs
```

The lab must be up and the movie must exist with both versions merged:

```bash
docker compose -f lab/docker-compose.yml -f lab/docker-compose.media.yml up -d
```

## What it measures

Each step prints the bytes and request count pulled from the byte-accounting
origin service, so a click can be correlated with what actually crossed the
relay. That is the part a human tester cannot easily observe, and it is how
seek behaviour will be judged: a working seek should issue a new range request
rather than refetching from zero.

## Caveat

This does not replace the manual test. It can prove the network behaviour and
that the UI renders, but a headless browser is not a television, and the
Android, Android TV, iOS and Roku rows of M-1 need real devices regardless.
