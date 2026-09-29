# Making a node reachable

Members of a group connect to each other at the address each one advertises
(assumption A-16). This guide covers what a node needs, the two accepted ways
to provide it, and how to tell whether it works.

## What a node needs

| Port | Protocol | Required | Why |
|---|---|---|---|
| 8443 (the federation port) | TCP | **Yes** | Members reach you here for your catalog, joining, group state, and to arrange direct paths. Media falls back to it. |
| 41641 (the direct-path port) | UDP | Recommended | Media between homes goes directly over it when both routers allow a hole punch. |

Set `JELLYMESH_PUBLIC_HOSTNAME` to the name or address, and port, where
others reach the TCP port, for example `mesh.example.org:8443`. When someone
joins, the approving node dials that address and refuses the join if the
joiner's key does not answer there. The message is "the joining node cannot
be reached at its advertised address".

## Route 1: a router port forward

This is the best route when you control every router between your server and
the internet: media goes directly between homes with no third party in the
path.

1. Give the server a fixed LAN address: a DHCP reservation in the router, or
   a static address.
2. Forward **TCP 8443** to the server's LAN address, port 8443.
3. Forward **UDP 41641** too, if you can. With it forwarded, direct paths
   need no punching at all.
4. Set `JELLYMESH_PUBLIC_HOSTNAME` to your public address or a DNS name that
   points at it, with the port.

**Two routers ("double NAT").** Many ISP installs put their own router in
front of yours. Look at your router's WAN or internet address:

- If it is a private address (`10.x.x.x`, `192.168.x.x`, or `172.16.x.x` to
  `172.31.x.x`), there is another router in front of it.
- Forward the same ports on that router too, pointing at your router's WAN
  address.
- Or put your router in the ISP router's DMZ.
- Or ask the ISP to put their router in bridge mode.

If you cannot log in to the ISP's router, use route 2.

**Carrier-grade NAT.** Your router's WAN address may be inside
`100.64.0.0/10` (`100.64.x.x` to `100.127.x.x`), or differ from the public
address a site such as `api.ipify.org` shows you, with no second router of
yours in between. In that case your ISP shares public addresses between
customers and no port forward can work. Use route 2.

## Route 2: Tailscale Funnel in raw-TCP mode

This is for homes that cannot forward a port. A free Tailscale account is
enough, and **members connecting to you do not need Tailscale**: Funnel
gives your server a public name.

```sh
tailscale funnel --bg --tcp 10000 tcp://localhost:8443
```

Then set `JELLYMESH_PUBLIC_HOSTNAME` to `<machine>.<tailnet>.ts.net:10000`.
Funnel allows only ports 443, 8443, and 10000.

**Use only `--tcp`.** Funnel's default HTTPS mode decrypts traffic at
Tailscale's servers. That breaks the pinned-key TLS every member relies on,
so members cannot connect and joining fails. `--tcp` passes the encrypted
connection through untouched.

Funnel carries every byte through Tailscale's servers, whose bandwidth limits
are not published. Catalogs and joining are small. Media still prefers a
direct UDP path between homes, arranged through the Funnel address, so most
playback does not go through Funnel at all.

## Direct paths (UDP)

Nothing to configure for most homes:

- The node learns its outside UDP address from public STUN servers
  (`JELLYMESH_STUN_SERVERS`).
- When a member plays one of your films, the two nodes punch a direct path.
- Until the path opens, and whenever it fails, media goes over the TCP port.
  Nobody watching notices the switch.

- **In Docker, publish the port:** add `"41641:41641/udp"` to `ports`, as
  `deploy/docker-compose.yml` does. Otherwise Docker's own NAT moves the
  port.
- **Members on your own network:** add your server's LAN address to
  `JELLYMESH_DIRECT_CANDIDATES` (for example `192.168.1.20:41641`), so a
  member in the same house connects locally.
- **Turning it off:** `JELLYMESH_DIRECT_LISTEN_ADDR=off`. Media then always
  uses TCP.

## Checking

**From outside your network.** Turn off Wi-Fi on a phone and open
`https://<your advertised address>`, for example
`https://mesh.example.org:8443`.

| What you see | What it means |
|---|---|
| A certificate warning, or an error about a missing certificate | Reachable: the node expects its own certificates |
| A page that spins until it times out | Blocked somewhere: check each router's forward, or use route 2 |

From inside your own network this test is unreliable, because many routers
cannot loop back to their own public address.

**`jellymesh status`** has a `direct` section:

| Field | Meaning |
|---|---|
| `outside_address` | Where your node appears to be on the internet |
| `varies_by_destination: true` | Your router gives each destination its own port, so direct paths cannot be punched. Media stays on TCP; forwarding UDP 41641 fixes it |
| `peers[].path` | `direct` or `tcp` for each member you have played from, with the last error if a direct path failed |

## Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| A join is refused with "cannot be reached at its advertised address" | The TCP port is not reachable from outside, or `JELLYMESH_PUBLIC_HOSTNAME` is wrong. Check from outside as above |
| A join is refused with "a different node answers" | The address or port forward points at another Jellymesh node. Fix the forward or the hostname |
| Members cannot connect through Funnel | Funnel is in HTTPS mode. Re-run it with `--tcp` |
| `direct` shows `tcp` with "no outside address to offer" | STUN servers are unreachable, or your NAT varies its mapping. Media works over TCP; forward UDP 41641 to allow direct paths |
| Everything works but media is slow through Funnel | Direct paths are not opening (see `peers[].last_error`). Forward UDP 41641 if you can |
