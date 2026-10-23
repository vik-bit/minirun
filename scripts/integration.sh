#!/usr/bin/env bash
# Runs the integration tests in a privileged container, so they need only Docker
# on the host: an Alpine root filesystem is exported from the alpine image, and the
# tests run as root inside the minirun-dev image (Go, iproute2, iptables).
set -euo pipefail
cd "$(dirname "$0")/.."

DOCKER=${DOCKER:-docker}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Docker on Windows (Git Bash) wants C:/... paths; pwd -W gives them there.
hostpath() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
export MSYS_NO_PATHCONV=1

"$DOCKER" build -q -t minirun-dev -f Dockerfile.dev . >/dev/null
id=$("$DOCKER" create alpine:3.20 true)
"$DOCKER" export "$id" > "$work/alpine.tar"
"$DOCKER" rm "$id" >/dev/null

# /tmp is a tmpfs: an overlay's upper layer cannot live on the container's own
# overlayfs root.
"$DOCKER" run --rm --privileged --tmpfs /tmp:rw,exec,size=2g \
  -v "$(hostpath .):/src" -v "$(hostpath "$work"):/work" \
  -v minirun-gomod:/go/pkg/mod -v minirun-gocache:/root/.cache/go-build \
  -e MINIRUN_IMAGE=/rootfs \
  minirun-dev sh -c 'mkdir -p /rootfs && tar -xf /work/alpine.tar -C /rootfs && go test -tags integration -count=1 -v ./tests/integration/ "$@"' sh "$@"
