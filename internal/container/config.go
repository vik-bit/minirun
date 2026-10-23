// Package container starts a process in its own namespaces, cgroup and root
// filesystem.
//
// A run has two halves in two processes:
//
//	parent (Run):  create the cgroup, re-exec itself with new namespaces straight
//	               into that cgroup, attach networking, then release the child.
//	child  (Init): mount an overlay root, pivot_root into it, set up /proc and /dev,
//	               drop capabilities, install seccomp, exec the user's command.
//
// The child blocks on a pipe until the parent says go, so the command never runs
// before its cgroup and network are in place.
package container

import "github.com/vik-bit/minirun/internal/cgroups"

// Config is everything the child needs. The parent sends it as JSON over a pipe.
type Config struct {
	ID       string   `json:"id"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Hostname string   `json:"hostname"`

	// Overlay root: the image is the read-only lower layer; writes go to Upper.
	Image  string `json:"image"`
	Upper  string `json:"upper"`
	Work   string `json:"work"`
	Merged string `json:"merged"`

	Seccomp bool `json:"seccomp"`

	Limits  cgroups.Limits `json:"limits"`
	Network Network        `json:"network"`
}

// Network says how the container is connected.
type Network struct {
	Mode       string `json:"mode"` // "none": loopback only; "cni": call a CNI plugin
	CNIConfig  string `json:"cni_config,omitempty"`
	CNIBinDirs string `json:"cni_bin_dirs,omitempty"`
}

// File descriptors the child inherits, after stdin, stdout and stderr.
const (
	fdConfig = 3
	fdSync   = 4
)

// InitArg is the hidden first argument that makes the binary run as a child.
const InitArg = "__minirun_init"
