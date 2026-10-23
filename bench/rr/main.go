// Command rr measures TCP request/response latency between two hosts, like
// netperf's TCP_RR: the client sends one byte, waits for one byte back, and
// repeats as fast as it can on a single connection. Each round trip is one
// transaction, and its latency is mostly network path plus scheduling, which is
// what a pod network adds to.
//
//	rr server [-addr :7000]
//	rr client -addr 10.244.1.2:7000 [-duration 10s] [-warmup 2s]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rr server|client [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	addr := fs.String("addr", ":7000", "listen address (server) or server address (client)")
	duration := fs.Duration("duration", 10*time.Second, "measured time")
	warmup := fs.Duration("warmup", 2*time.Second, "unmeasured time first, to settle caches and ARP")
	_ = fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "server":
		serve(*addr)
	case "client":
		client(*addr, *warmup, *duration)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

func serve(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			c.(*net.TCPConn).SetNoDelay(true)
			buf := make([]byte, 1)
			for {
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				if _, err := c.Write(buf); err != nil {
					return
				}
			}
		}(c)
	}
}

func client(addr string, warmup, duration time.Duration) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()
	c.(*net.TCPConn).SetNoDelay(true) // no Nagle delay on 1-byte writes
	buf := []byte{'x'}
	roundTrip := func() time.Duration {
		start := time.Now()
		if _, err := c.Write(buf); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return time.Since(start)
	}
	for end := time.Now().Add(warmup); time.Now().Before(end); {
		roundTrip()
	}
	lat := make([]time.Duration, 0, 1<<20)
	start := time.Now()
	for time.Since(start) < duration {
		lat = append(lat, roundTrip())
	}
	elapsed := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 { return float64(lat[int(p*float64(len(lat)-1))].Microseconds()) }
	_ = json.NewEncoder(os.Stdout).Encode(map[string]float64{
		"transactions": float64(len(lat)), "per_second": float64(len(lat)) / elapsed.Seconds(),
		"p50_us": pct(0.50), "p90_us": pct(0.90), "p99_us": pct(0.99), "p999_us": pct(0.999),
	})
}
