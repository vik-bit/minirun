#!/usr/bin/env bash
# Pod-to-pod network benchmark on a Kind cluster that already has a CNI installed.
# A server pod on one worker; a client pod on the other worker (cross-node) and
# one on the same worker (same-node). Each repetition measures TCP throughput
# (iperf3, 10 s) and request/response latency (rr, 10 s), and appends one JSON
# line per measurement to bench/results/net-<label>.jsonl.
#
# Usage: scripts/bench-net.sh <label> [repetitions]
set -euo pipefail
cd "$(dirname "$0")/.."

LABEL=$1 REPS=${2:-5}
KIND=${KIND:-kind} KUBECTL=${KUBECTL:-kubectl} DOCKER=${DOCKER:-docker} CLUSTER=${CLUSTER:-minicni}
PYTHON=${PYTHON:-python3}
k() { "$KUBECTL" --context "kind-$CLUSTER" "$@"; }
out=bench/results/net-$LABEL.jsonl
mkdir -p bench/results && : > "$out"

"$DOCKER" build -q -t minirun-bench:dev -f bench/Dockerfile . >/dev/null
"$KIND" load docker-image minirun-bench:dev --name "$CLUSTER" >/dev/null

workers=($(k get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}'))
k delete pod bench-server bench-cross bench-same --ignore-not-found --wait >/dev/null
pod() { # name node command...
  local name=$1 node=$2; shift 2
  k run "$name" --image=minirun-bench:dev --image-pull-policy=Never --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"$node\"}}" -- "$@" >/dev/null
}
pod bench-server "${workers[1]}" sh -c 'iperf3 -s -D && rr server'
pod bench-cross "${workers[0]}" sleep 7200
pod bench-same "${workers[1]}" sleep 7200
k wait --for=condition=Ready pod/bench-server pod/bench-cross pod/bench-same --timeout=180s >/dev/null
ip=$(k get pod bench-server -o jsonpath='{.status.podIP}')
echo "$LABEL: server $ip on ${workers[1]}"

for rep in $(seq "$REPS"); do
  for path in cross same; do
    client=bench-$path
    gbps=$(k exec "$client" -- iperf3 -c "$ip" -t 10 -J |
      "$PYTHON" -c 'import json,sys; print(json.load(sys.stdin)["end"]["sum_received"]["bits_per_second"]/1e9)')
    rr=$(k exec "$client" -- rr client -addr "$ip:7000" -duration 10s)
    line="{\"cni\":\"$LABEL\",\"path\":\"$path\",\"rep\":$rep,\"gbps\":$gbps,\"rr\":$rr}"
    echo "$line" >> "$out"
    echo "  rep $rep $path: $(printf '%.2f' "$gbps") Gbit/s, rr $rr"
  done
done
k delete pod bench-server bench-cross bench-same --wait=false >/dev/null
echo "wrote $out"
