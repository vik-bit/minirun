"""Turns bench/results/*.jsonl into bench/results/summary.md.

Network: median and range over the repetitions, per CNI and path.
Start time: median and p95 over the runs, per runtime.
"""

import json
import statistics
from pathlib import Path

RESULTS = Path(__file__).parent / "results"
ORDER = ["minicni", "flannel-host-gw", "flannel-vxlan"]
NAMES = {"minicni": "minicni (host-gw)", "flannel-host-gw": "Flannel host-gw", "flannel-vxlan": "Flannel VXLAN (default)"}


def spread(values, fmt):
    return f"{fmt.format(statistics.median(values))} ({fmt.format(min(values))}–{fmt.format(max(values))})"


def network():
    rows = {}
    for path in sorted(RESULTS.glob("net-*.jsonl")):
        for line in path.read_text().splitlines():
            r = json.loads(line)
            rows.setdefault((r["cni"], r["path"]), []).append(r)
    lines = ["| CNI | Path | Throughput, Gbit/s | Latency p50, µs | Latency p99, µs | Round trips/s |",
             "|---|---|---|---|---|---|"]
    for cni in ORDER:
        for path in ("cross", "same"):
            reps = rows.get((cni, path))
            if not reps:
                continue
            lines.append(f"| {NAMES[cni]} | {'cross-node' if path == 'cross' else 'same-node'} | "
                         f"{spread([r['gbps'] for r in reps], '{:.1f}')} | "
                         f"{spread([r['rr']['p50_us'] for r in reps], '{:.0f}')} | "
                         f"{spread([r['rr']['p99_us'] for r in reps], '{:.0f}')} | "
                         f"{spread([r['rr']['per_second'] for r in reps], '{:,.0f}')} |")
    return lines, len(next(iter(rows.values())))


def start():
    lines = ["| Runtime | Median, ms | p95, ms | Min–max, ms |", "|---|---|---|---|"]
    for line in (RESULTS / "start.jsonl").read_text().splitlines():
        if not line.startswith("{"):
            continue
        r = json.loads(line)
        lines.append(f"| {r['name']} | {r['median_ms']:.1f} | {r['p95_ms']:.1f} | {r['min_ms']:.1f}–{r['max_ms']:.1f} |")
    return lines


def main():
    net, reps = network()
    out = ["# Benchmark results", "",
           f"Pod to pod on a three-node Kind cluster, {reps} repetitions per row: iperf3 for 10 s, then a "
           "1-byte TCP request/response loop for 10 s. Cells show the median and, in brackets, the range.", ""]
    out += net + ["", "Container start-to-exit time for `/bin/true`, interleaved runs:", ""] + start() + [""]
    (RESULTS / "summary.md").write_text("\n".join(out), encoding="utf-8")
    print("\n".join(out))


if __name__ == "__main__":
    main()
