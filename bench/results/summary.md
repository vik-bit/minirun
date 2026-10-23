# Benchmark results

Pod to pod on a three-node Kind cluster, 5 repetitions per row: iperf3 for 10 s, then a 1-byte TCP request/response loop for 10 s. Cells show the median and, in brackets, the range.

| CNI | Path | Throughput, Gbit/s | Latency p50, µs | Latency p99, µs | Round trips/s |
|---|---|---|---|---|---|
| minicni (host-gw) | cross-node | 24.1 (22.8–25.2) | 171 (170–179) | 511 (460–545) | 5,498 (5,155–5,586) |
| minicni (host-gw) | same-node | 35.5 (35.3–36.6) | 145 (140–151) | 462 (448–471) | 6,415 (6,156–6,536) |
| Flannel host-gw | cross-node | 24.5 (21.4–24.7) | 174 (171–199) | 544 (528–579) | 5,313 (4,630–5,434) |
| Flannel host-gw | same-node | 35.1 (31.7–35.3) | 152 (147–192) | 492 (470–554) | 6,101 (4,758–6,227) |
| Flannel VXLAN (default) | cross-node | 13.2 (13.1–13.8) | 218 (213–221) | 581 (550–599) | 4,246 (4,157–4,264) |
| Flannel VXLAN (default) | same-node | 26.9 (26.4–27.3) | 180 (179–181) | 466 (458–489) | 5,322 (5,235–5,363) |

Container start-to-exit time for `/bin/true`, interleaved runs:

| Runtime | Median, ms | p95, ms | Min–max, ms |
|---|---|---|---|
| runc | 48.2 | 69.5 | 33.9–121.8 |
| minirun | 9.2 | 14.9 | 7.1–47.7 |
| minirun-no-seccomp | 8.2 | 22.3 | 6.7–35.7 |
