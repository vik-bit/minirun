#!/usr/bin/env bash
# Container start-to-exit time for /bin/true: minirun against runc, from the same
# Alpine root filesystem, interleaved, in a privileged container. Writes
# bench/results/start.jsonl.
#
# The runc bundle is `runc spec`'s default: new namespaces, a cgroup, masked paths
# and dropped capabilities, but no seccomp profile. minirun is therefore measured
# both with its seccomp filter and without it.
set -euo pipefail
cd "$(dirname "$0")/.."

DOCKER=${DOCKER:-docker} RUNS=${RUNS:-50}
hostpath() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
export MSYS_NO_PATHCONV=1
work=$(mktemp -d) && trap 'rm -rf "$work"' EXIT

"$DOCKER" build -q -t minirun-dev -f Dockerfile.dev . >/dev/null
id=$("$DOCKER" create alpine:3.20 true) && "$DOCKER" export "$id" > "$work/alpine.tar" && "$DOCKER" rm "$id" >/dev/null
mkdir -p bench/results

"$DOCKER" run --rm --privileged --tmpfs /tmp:rw,exec,size=2g -e RUNS="$RUNS" \
  -v "$(hostpath .):/src" -v "$(hostpath "$work"):/work" \
  -v minirun-gomod:/go/pkg/mod -v minirun-gocache:/root/.cache/go-build \
  minirun-dev sh -ec '
    go build -buildvcs=false -o /tmp/minirun ./cmd/minirun && go build -buildvcs=false -o /tmp/starttime ./bench/starttime
    # As on a systemd host, keep the root cgroup free of processes, so runc and
    # minirun both start from the same clean state.
    mkdir -p /sys/fs/cgroup/init && for p in $(cat /sys/fs/cgroup/cgroup.procs); do echo $p > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
    mkdir -p /tmp/bundle/rootfs && tar -xf /work/alpine.tar -C /tmp/bundle/rootfs
    cd /tmp/bundle && runc spec
    sed -i "s/\"terminal\": true/\"terminal\": false/; s/\"sh\"/\"\/bin\/true\"/" config.json
    runc --version | head -1
    /tmp/starttime -n "$RUNS" \
      -- runc "runc run -b /tmp/bundle bench-\$\$" \
      -- minirun "/tmp/minirun run --image /tmp/bundle/rootfs --state-dir /tmp/state -- /bin/true" \
      -- minirun-no-seccomp "/tmp/minirun run --no-seccomp --image /tmp/bundle/rootfs --state-dir /tmp/state -- /bin/true"
  ' | tee bench/results/start.jsonl
