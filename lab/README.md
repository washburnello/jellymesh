# Phase 0 lab

This directory contains a safe starting point for two disposable Jellyfin instances on Walnut.

The compose file intentionally:

- binds only to loopback;
- uses the same Jellyfin version as Cedar;
- keeps all data under `lab/data/`;
- does not mount media;
- does not expose a public port;
- does not include a federation service yet.

## Start the lab

```bash
docker compose -f lab/docker-compose.yml up -d
```

Then open:

```text
http://127.0.0.1:18096
http://127.0.0.1:18097
```

Complete first-run setup separately for each instance. Do not use production credentials or copy production databases.

## Stop the lab

```bash
docker compose -f lab/docker-compose.yml down
```

The `down` command does not remove the persistent lab data. Do not use `down -v` unless you intentionally want to delete the lab state.

## Status

```bash
docker compose -f lab/docker-compose.yml ps
docker logs jellymesh-jellyfin-peer-a
docker logs jellymesh-jellyfin-peer-b
```

## Next implementation step

The next safe step is to create the Jellymesh Service skeleton with configuration validation, persistent state, health/readiness endpoints, and a deliberately empty federation adapter. It should run in `disabled` mode until the Phase 0 synthetic libraries and publication/acceptance policy are ready.

## Adapter check (M-7)

`lab/adaptercheck` runs Jellymesh's Jellyfin adapter against a real Jellyfin
and reports each route it depends on. `run-throwaway.sh` does the whole
procedure against a fresh container that it removes afterwards, with online
metadata disabled and random credentials that are never printed:

```bash
./lab/adaptercheck/run-throwaway.sh "lab/media/local-movies/<any>.mkv"
```

Run it after every Jellyfin upgrade.
