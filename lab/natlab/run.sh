#!/usr/bin/env bash
# The NAT lab (#54, C-NT-4): does a direct QUIC path open through home NATs?
# Everything runs on isolated Docker networks with no route to the real LAN
# or the internet:
#
#   nl-internet 10.99.0.0/24   two STUN servers (.10, .11) and the routers' outside addresses
#   nl-home-a   10.98.1.0/24   router-a (.2) and node a (.10), or router-a2 (.3) for double NAT
#   nl-home-a2  10.98.11.0/24  router-a2 (.2) and node a (.10): the inner home of a double NAT
#   nl-home-b   10.98.2.0/24   router-b (.2) and node b (.10)
#
# A router is "easy" (MASQUERADE, which keeps a socket's outside port for
# every destination) or "hard" (MASQUERADE --random-fully, a new outside
# port per destination). Docker gives each network's .1 to the host bridge,
# so routers take .2. b's router forwards TCP 8443 to b, its advertised
# address, where a sends its offer; both then punch at the start time in
# b's answer. DELAY_MS (default 15) delays each router's outside link, to
# stand in for the internet between homes. Each scenario prints what the
# nodes reported.
#
#   ./lab/natlab/run.sh            run every scenario
#   ./lab/natlab/run.sh easy easy  run one: <router a> <router b> [double]
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
nets=(nl-internet nl-home-a nl-home-a2 nl-home-b)
cleanup() {
  docker rm -f nl-stun1 nl-stun2 nl-router-a nl-router-a2 nl-router-b nl-node-a nl-node-b >/dev/null 2>&1 || true
  for n in "${nets[@]}"; do docker network rm "$n" >/dev/null 2>&1 || true; done
  rm -rf "$work"
}
trap cleanup EXIT

(cd "$root" && CGO_ENABLED=0 PATH=$HOME/.local/share/mise/installs/go/1.27.1/bin:$PATH go build -trimpath -o "$work/natpunch" ./lab/natlab/natpunch)
printf 'FROM alpine:3\nRUN apk add --no-cache iptables iproute2 tcpdump conntrack-tools >/dev/null\nCOPY natpunch /usr/local/bin/natpunch\n' |
  docker build -q -t jellymesh-natlab:latest -f - "$work" >/dev/null

network() { docker network create --internal --subnet "$2" "$1" >/dev/null; }
iface_for() { docker exec "$1" sh -c "ip -o -4 addr | awk '/ $2\\//{print \$2}'"; }

router() { # name outside-net outside-ip inside-net inside-ip kind [upstream-gateway]
  local name=$1 outnet=$2 outip=$3 innet=$4 inip=$5 kind=$6 upstream=${7:-}
  docker run -d --name "$name" --cap-add NET_ADMIN --sysctl net.ipv4.ip_forward=1 \
    --network "$outnet" --ip "$outip" jellymesh-natlab:latest sleep infinity >/dev/null
  docker network connect --ip "$inip" "$innet" "$name"
  local out; out=$(iface_for "$name" "$outip")
  local random=""; [ "$kind" = hard ] && random="--random-fully"
  docker exec "$name" sh -c "iptables -t nat -A POSTROUTING -o $out -j MASQUERADE $random"
  # DELAY_MS emulates the internet between homes, one way, per router.
  [ "${DELAY_MS:-15}" != 0 ] && docker exec "$name" tc qdisc add dev "$out" root netem delay "${DELAY_MS:-15}ms"
  [ -n "$upstream" ] && docker exec "$name" ip route replace default via "$upstream"
  return 0
}

scenario() { # kind-a kind-b [double]
  local a=$1 b=$2 double=${3:-}
  local label="a:$a${double:+ (double NAT)} b:$b"
  for n in "${nets[@]}"; do docker network rm "$n" >/dev/null 2>&1 || true; done
  network nl-internet 10.99.0.0/24; network nl-home-a 10.98.1.0/24
  network nl-home-a2 10.98.11.0/24; network nl-home-b 10.98.2.0/24
  mkdir -p "$work/shared" && rm -f "$work/shared"/*
  chmod 777 "$work/shared"
  for i in 1 2; do
    docker run -d --name "nl-stun$i" --network nl-internet --ip "10.99.0.1$((i-1))" jellymesh-natlab:latest natpunch stun >/dev/null
  done
  router nl-router-a nl-internet 10.99.0.21 nl-home-a 10.98.1.2 "$a"
  router nl-router-b nl-internet 10.99.0.22 nl-home-b 10.98.2.2 "$b"
  # b's advertised address: a port forward for signalling, as A-16 requires.
  docker exec nl-router-b sh -c "iptables -t nat -A PREROUTING -i $(iface_for nl-router-b 10.99.0.22) -p tcp --dport 8443 -j DNAT --to-destination 10.98.2.10:8443"
  local home=nl-home-a gateway=10.98.1.2
  if [ -n "$double" ]; then
    router nl-router-a2 nl-home-a 10.98.1.3 nl-home-a2 10.98.11.2 easy 10.98.1.2
    home=nl-home-a2 gateway=10.98.11.2
  fi
  local subnet=${home#nl-home-}
  local node_ip; node_ip=$([ "$home" = nl-home-a2 ] && echo 10.98.11.10 || echo 10.98.1.10)
  local stun="10.99.0.10:3478,10.99.0.11:3478"
  docker run -d --name nl-node-a --cap-add NET_ADMIN --network "$home" --ip "$node_ip" -v "$work/shared:/shared" \
    jellymesh-natlab:latest sh -c "ip route replace default via $gateway && natpunch node -name a -peer b -role client -stun $stun -signal 10.99.0.22:8443" >/dev/null
  docker run -d --name nl-node-b --cap-add NET_ADMIN --network nl-home-b --ip 10.98.2.10 -v "$work/shared:/shared" \
    jellymesh-natlab:latest sh -c "ip route replace default via 10.98.2.2 && natpunch node -name b -peer a -role server -stun $stun" >/dev/null
  for _ in $(seq 1 90); do [ -f "$work/shared/a.result" ] && [ -f "$work/shared/b.result" ] && break; sleep 1; done
  printf '%-34s a %s\n%-34s b %s\n' "$label" "$(cat "$work/shared/a.result" 2>/dev/null || echo 'no result')" "" "$(cat "$work/shared/b.result" 2>/dev/null || echo 'no result')"
  [ -n "${KEEP:-}" ] && { echo "KEEP set: containers left running"; trap - EXIT; return; }
  docker rm -f nl-stun1 nl-stun2 nl-router-a nl-router-a2 nl-router-b nl-node-a nl-node-b >/dev/null 2>&1 || true
  : "$subnet"
}

if [ $# -ge 2 ]; then
  scenario "$@"
else
  scenario easy easy
  scenario easy easy double
  scenario hard easy
  scenario hard hard
fi
