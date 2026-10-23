// Package cni implements the Container Network Interface 1.0 contract
// (https://github.com/containernetworking/cni/blob/main/SPEC.md): the types a
// plugin reads and writes, and the runtime side that invokes plugins.
package cni

import (
	"encoding/json"
	"fmt"
)

// Version is the spec version this code speaks.
const Version = "1.0.0"

// SupportedVersions answers the VERSION command.
var SupportedVersions = []string{"1.0.0"}

// Interface is one link the plugin created or configured.
type Interface struct {
	Name    string `json:"name"`
	Mac     string `json:"mac,omitempty"`
	Sandbox string `json:"sandbox,omitempty"` // set for interfaces inside the container
}

// IPConfig is an address assigned to one of the Interfaces (by index).
type IPConfig struct {
	Address   string `json:"address"` // CIDR, e.g. 10.244.1.5/24
	Gateway   string `json:"gateway,omitempty"`
	Interface *int   `json:"interface,omitempty"`
}

// Route is a route added inside the container.
type Route struct {
	Dst string `json:"dst"`
	GW  string `json:"gw,omitempty"`
}

// Result is what a successful ADD prints.
type Result struct {
	CNIVersion string      `json:"cniVersion"`
	Interfaces []Interface `json:"interfaces,omitempty"`
	IPs        []IPConfig  `json:"ips,omitempty"`
	Routes     []Route     `json:"routes,omitempty"`
	DNS        struct{}    `json:"dns"`
}

// Error is what a failing plugin prints, with a non-zero exit status.
type Error struct {
	CNIVersion string `json:"cniVersion"`
	Code       uint   `json:"code"`
	Msg        string `json:"msg"`
	Details    string `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("cni error %d: %s: %s", e.Code, e.Msg, e.Details)
	}
	return fmt.Sprintf("cni error %d: %s", e.Code, e.Msg)
}

// Error codes defined by the spec.
const (
	ErrIncompatibleVersion = 1
	ErrInvalidEnvironment  = 4
	ErrIOFailure           = 5
	ErrDecodingFailure     = 6
	ErrInvalidConfig       = 7
	ErrTryAgainLater       = 11
)

// conflist is a network configuration list: one network, a chain of plugins.
type conflist struct {
	CNIVersion string            `json:"cniVersion"`
	Name       string            `json:"name"`
	Plugins    []json.RawMessage `json:"plugins"`
}
