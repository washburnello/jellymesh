# FUSE gate lab (#73)

The FUSE spike's gates (#60, `lab/fusespike/`), rerun against the real
build. What the spike's stand-ins did is done here by the product:

```text
source Jellyfin -> fault proxy -> source node (fl-src)
    -> destination node's read service (fl-dst, JELLYMESH_PRESENTATION=fuse)
    -> jellymesh mount (fl-mount) -> destination Jellyfin (fl-dst-jf, uid 7777)
```

- `env.sh up` builds the image, hard-links the three Back to the Future
  films and 30 synthetic copies (other titles and identities) into the
  source's library, starts everything, forms a group (the destination
  founds it, the source joins and publishes), materializes, and adds the
  mount's films to the destination Jellyfin as "Jellymesh Movies" with
  extraction off.
- `env.sh down` removes every container, volume, and mount.
- `gates.py g1|g2|g3|g3_extraction|g6|g7` runs a gate.
- `faultproxy/` sits between the source node and its Jellyfin. It counts
  media bytes and injects the spike's faults: refuse, hang, crawl, and
  latency.

Everything runs on walnut, in its own containers and
`~/.local/share/jellymesh-fuselab`. The destination Jellyfin is on
`127.0.0.1:18230` and the LAN (`192.168.87.20:18230`), for G4 on real
devices. Its credentials are in `credentials` there and are never printed.

Differences from the spike:
- No `.strm` library side by side: a node has one presentation. The G6
  `.strm` figures are the spike's.
- The "reader" is the destination node itself, so "reader stopped" stops
  the node.
- The source's upload ceiling is off (`JELLYMESH_UPLOAD_CEILING_MBPS=0`),
  so G6 measures the chain, not the ceiling.
- Past each film's burst, reads as fast as possible are paced (#69). G6
  reports throughput inside the burst and the paced rate past it.

## Results

See the end of this file once runs are recorded.
