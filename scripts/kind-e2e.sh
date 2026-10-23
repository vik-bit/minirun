#!/usr/bin/env bash
# End-to-end test on a three-node Kind cluster whose only network plugin is
# minicni. Each check passes only if pod networking works:
#   1. all nodes become Ready (the kubelet found a working CNI config);
#   2. CoreDNS starts (its pods need addresses);
#   3. a pod pings a pod on its own node (through the bridge) and on another node
#      (through a host-gw route the daemon installed);
#   4. a pod fetches a page through a Service, by DNS name (kube-proxy + CoreDNS).
#
# Usage: scripts/kind-e2e.sh [--keep]   (--keep leaves the cluster running)
set -euo pipefail
cd "$(dirname "$0")/.."

KIND=${KIND:-kind} KUBECTL=${KUBECTL:-kubectl} DOCKER=${DOCKER:-docker}
CLUSTER=${CLUSTER:-minicni}
k() { "$KUBECTL" --context "kind-$CLUSTER" "$@"; }
step() { printf '\n== %s\n' "$*"; }
ok() { printf 'PASS  %s\n' "$*"; }

if [[ "${1:-}" != "--keep" ]]; then
  trap '"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true' EXIT
fi

step "create cluster (no CNI)"
"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
"$KIND" create cluster --name "$CLUSTER" --config deploy/kind.yaml --wait 0s
k get nodes

step "build and load the minicni image"
"$DOCKER" build -q -t minicni:dev . >/dev/null
"$KIND" load docker-image minicni:dev --name "$CLUSTER"

step "install minicni"
k apply -f deploy/minicni.yaml
k -n kube-system rollout status daemonset/minicni --timeout=120s
k wait --for=condition=Ready nodes --all --timeout=180s
ok "all nodes Ready"
k get nodes -o custom-columns=NAME:.metadata.name,POD-CIDR:.spec.podCIDR,STATUS:.status.conditions[-1].type

k -n kube-system rollout status deployment/coredns --timeout=180s
ok "CoreDNS running"

step "deploy test pods"
workers=($(k get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}'))
pod() { # name node command...
  local name=$1 node=$2; shift 2
  k run "$name" --image=busybox:1.36 --restart=Never --labels="app=$name" \
    --overrides="{\"spec\":{\"nodeName\":\"$node\"}}" -- "$@" >/dev/null
}
pod server "${workers[1]}" sh -c 'mkdir -p /www && echo hello-from-server > /www/index.html && httpd -f -p 8080 -h /www'
pod same-node "${workers[1]}" sleep 3600
pod other-node "${workers[0]}" sleep 3600
k expose pod server --port 80 --target-port 8080 >/dev/null
k wait --for=condition=Ready pod/server pod/same-node pod/other-node --timeout=180s
k get pods -o wide

server_ip=$(k get pod server -o jsonpath='{.status.podIP}')

step "connectivity"
k exec same-node -- ping -c 3 -W 2 "$server_ip" >/dev/null
ok "same-node pod -> $server_ip (through the bridge)"
k exec other-node -- ping -c 3 -W 2 "$server_ip" >/dev/null
ok "cross-node pod -> $server_ip (through a host-gw route)"
body=$(k exec other-node -- wget -qO- -T 5 http://server.default.svc.cluster.local/)
[[ "$body" == "hello-from-server" ]]
ok "Service by DNS name returned '$body'"

step "routes on ${workers[0]}"
"$DOCKER" exec "${workers[0]}" ip route | grep -E '10\.244\.' || true

printf '\nAll end-to-end checks passed.\n'
