// Command starttime measures how long each of several commands takes to run,
// interleaving them (A, B, C, A, B, C, ...) so that slow drift in the machine,
// such as heat or power limits, affects every command equally.
//
//	starttime -n 50 -- name1 'command 1' -- name2 'command 2'
//
// Prints one JSON object per command with the median, p95, min and max in ms.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

func main() {
	n := flag.Int("n", 50, "runs per command")
	warmup := flag.Int("warmup", 3, "unmeasured runs per command first")
	flag.Parse()
	type job struct {
		name, cmd string
		ms        []float64
	}
	var jobs []*job
	// flag stops at, and swallows, the first "--"; the rest are separated by "--".
	var group []string
	for _, a := range append(flag.Args(), "--") {
		if a != "--" {
			group = append(group, a)
			continue
		}
		if len(group) == 0 {
			continue
		}
		if len(group) != 2 {
			fmt.Fprintln(os.Stderr, "usage: starttime -n N -- name 'command' [-- name 'command' ...]")
			os.Exit(2)
		}
		jobs = append(jobs, &job{name: group[0], cmd: group[1]})
		group = nil
	}
	runOnce := func(j *job) time.Duration {
		start := time.Now()
		out, err := exec.Command("sh", "-c", j.cmd).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s failed: %v\n%s", j.name, err, out)
			os.Exit(1)
		}
		return time.Since(start)
	}
	for i := 0; i < *warmup; i++ {
		for _, j := range jobs {
			runOnce(j)
		}
	}
	for i := 0; i < *n; i++ {
		for _, j := range jobs {
			j.ms = append(j.ms, float64(runOnce(j).Microseconds())/1000)
		}
	}
	for _, j := range jobs {
		sort.Float64s(j.ms)
		at := func(p float64) float64 { return j.ms[int(p*float64(len(j.ms)-1))] }
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"name": j.name, "runs": len(j.ms), "median_ms": at(0.5), "p95_ms": at(0.95),
			"min_ms": j.ms[0], "max_ms": j.ms[len(j.ms)-1],
		})
	}
}
