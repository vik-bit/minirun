// Package cgroups manages a cgroup v2 group per container: limits, membership and
// usage counters.
package cgroups

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Root is the unified cgroup hierarchy mount point.
const Root = "/sys/fs/cgroup"

var controllers = []string{"cpu", "memory", "pids"}

// Limits for one container. Zero values mean "no limit".
type Limits struct {
	MemoryBytes int64   // memory.max; swap is disabled so the limit is real
	CPUs        float64 // cpu.max quota as a fraction of one CPU per period
	Pids        int64   // pids.max
}

// Group is one container's cgroup directory.
type Group struct {
	Path string
}

// Create makes <Root>/<parent>/<name> with the given limits and returns an open
// directory fd for it, which the caller passes to clone(CLONE_INTO_CGROUP) so the
// container's first process starts inside its limits rather than being moved later.
func Create(parent, name string, l Limits) (*Group, *os.File, error) {
	if err := enableControllers(filepath.Join(Root, parent)); err != nil {
		return nil, nil, err
	}
	g := &Group{Path: filepath.Join(Root, parent, name)}
	if err := os.Mkdir(g.Path, 0o755); err != nil {
		return nil, nil, fmt.Errorf("cgroups: create %s: %w", g.Path, err)
	}
	if err := g.apply(l); err != nil {
		g.Remove()
		return nil, nil, err
	}
	dir, err := os.Open(g.Path)
	if err != nil {
		g.Remove()
		return nil, nil, err
	}
	return g, dir, nil
}

func (g *Group) apply(l Limits) error {
	if l.MemoryBytes > 0 {
		if err := g.write("memory.max", strconv.FormatInt(l.MemoryBytes, 10)); err != nil {
			return err
		}
		// Without this, a memory-limited process just pages to swap and slows down
		// instead of hitting the limit. Missing when the host has no swap: fine.
		if err := g.write("memory.swap.max", "0"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if l.CPUs > 0 {
		const period = 100_000 // microseconds
		quota := int64(math.Round(l.CPUs * period))
		if err := g.write("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return err
		}
	}
	if l.Pids > 0 {
		if err := g.write("pids.max", strconv.FormatInt(l.Pids, 10)); err != nil {
			return err
		}
	}
	return nil
}

// enableControllers turns on cpu, memory and pids for children of every directory
// from Root down to dir.
//
// cgroup v2 forbids enabling controllers for children of a group that itself holds
// processes ("no internal processes"). Inside a Docker or Kind container, the
// namespace's root group is exactly that. Like runc and systemd, move those
// processes into a leaf group named "init" first.
func enableControllers(dir string) error {
	rel, err := filepath.Rel(Root, dir)
	if err != nil {
		return err
	}
	cur := Root
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := 0; ; i++ {
		if err := enableHere(cur); err != nil {
			return err
		}
		if i == len(parts) {
			return nil
		}
		cur = filepath.Join(cur, parts[i])
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
}

func enableHere(dir string) error {
	want := "+" + strings.Join(controllers, " +")
	err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(want), 0)
	if !errors.Is(err, unix.EBUSY) {
		if err != nil {
			return fmt.Errorf("cgroups: enable controllers in %s: %w", dir, err)
		}
		return nil
	}
	leaf := filepath.Join(dir, "init")
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, pid := range strings.Fields(string(procs)) {
		// A process may exit between reading the list and moving it.
		if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(pid), 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("cgroups: move pid %s to %s: %w", pid, leaf, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(want), 0); err != nil {
		return fmt.Errorf("cgroups: enable controllers in %s: %w", dir, err)
	}
	return nil
}

func (g *Group) write(file, value string) error {
	if err := os.WriteFile(filepath.Join(g.Path, file), []byte(value), 0); err != nil {
		return fmt.Errorf("cgroups: write %s=%q: %w", file, value, err)
	}
	return nil
}

// OOMKills reports how many times the kernel killed a process for exceeding memory.max.
func (g *Group) OOMKills() int64 {
	return keyedValue(filepath.Join(g.Path, "memory.events"), "oom_kill")
}

// PeakMemory is the highest memory usage the group reached, if the kernel records it.
func (g *Group) PeakMemory() int64 {
	b, err := os.ReadFile(filepath.Join(g.Path, "memory.peak"))
	if err != nil {
		return -1
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// Remove deletes the group. It must be empty: the container's processes have exited.
func (g *Group) Remove() error {
	return os.Remove(g.Path)
}

func keyedValue(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 2 && fields[0] == key {
			n, _ := strconv.ParseInt(fields[1], 10, 64)
			return n
		}
	}
	return -1
}

// ParseBytes accepts sizes like "512", "64k", "64M" or "1G" (powers of 1024).
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch strings.ToLower(s[len(s)-1:]) {
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
