#!/usr/bin/env bash
# Creates the same three-node Kind cluster as kind-e2e.sh, with Flannel instead
# of minicni, for comparison.
#
# Usage: scripts/cluster-flannel.sh host-gw|vxlan
#   host-gw: the same design as minicni (routes to each node, no encapsulation)
#   vxlan:   Flannel's default (pod packets wrapped in UDP between nodes)
set -euo pipefail
cd "$(dirname "$0")/.."

BACKEND=$1
KIND=${KIND:-kind} KUBECTL=${KUBECTL:-kubectl} DOCKER=${DOCKER:-docker} CLUSTER=${CLUSTER:-minicni}
FLANNEL=v0.25.6 PLUGINS=v1.5.1
k() { "$KUBECTL" --context "kind-$CLUSTER" "$@"; }
cache=.tools/cache && mkdir -p "$cache"

"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
"$KIND" create cluster --name "$CLUSTER" --config deploy/kind.yaml --wait 0s

# Flannel's plugin delegates to the standard "bridge" plugin, which Kind's node
# image does not include.
tgz=$cache/cni-plugins-$PLUGINS.tgz
[[ -f $tgz ]] || curl -sSL -o "$tgz" "https://github.com/containernetworking/plugins/releases/download/$PLUGINS/cni-plugins-linux-amd64-$PLUGINS.tgz"
tar -xzf "$tgz" -C "$cache" ./bridge
for node in $("$KIND" get nodes --name "$CLUSTER"); do
  "$DOCKER" cp "$cache/bridge" "$node:/opt/cni/bin/bridge"
done

manifest=$cache/kube-flannel-$FLANNEL.yml
[[ -f $manifest ]] || curl -sSL -o "$manifest" "https://github.com/flannel-io/flannel/releases/download/$FLANNEL/kube-flannel.yml"
sed "s/\"Type\": \"vxlan\"/\"Type\": \"$BACKEND\"/" "$manifest" | k apply -f -
k -n kube-flannel rollout status daemonset/kube-flannel-ds --timeout=180s
k wait --for=condition=Ready nodes --all --timeout=180s
k -n kube-system rollout status deployment/coredns --timeout=180s
echo "Flannel ($BACKEND) ready"
