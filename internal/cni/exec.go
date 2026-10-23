package cni

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Invocation is one runtime-to-plugin call.
type Invocation struct {
	Command     string // ADD, DEL or CHECK
	ContainerID string
	NetNS       string // path to the network namespace, e.g. /proc/123/ns/net
	IfName      string // interface name inside the container
	Path        string // colon-separated directories to search for plugin binaries
	Config      []byte // a network configuration list (a single config is accepted too)
}

// Exec runs every plugin in the configuration list, as a container runtime does.
// ADD runs them in order, passing each one the previous result; DEL runs them in
// reverse and keeps going past errors so cleanup is as complete as possible.
func Exec(inv Invocation) (*Result, error) {
	var list conflist
	if err := json.Unmarshal(inv.Config, &list); err != nil {
		return nil, fmt.Errorf("cni: parse network config: %w", err)
	}
	plugins := list.Plugins
	if len(plugins) == 0 { // a single plugin config rather than a list
		plugins = []json.RawMessage{inv.Config}
	}

	if inv.Command == "DEL" {
		var firstErr error
		for i := len(plugins) - 1; i >= 0; i-- {
			if _, err := execPlugin(inv, list, plugins[i], nil); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return nil, firstErr
	}
	var prev *Result
	for _, p := range plugins {
		res, err := execPlugin(inv, list, p, prev)
		if err != nil {
			return nil, err
		}
		if res != nil {
			prev = res
		}
	}
	return prev, nil
}

func execPlugin(inv Invocation, list conflist, raw json.RawMessage, prev *Result) (*Result, error) {
	var conf map[string]any
	if err := json.Unmarshal(raw, &conf); err != nil {
		return nil, fmt.Errorf("cni: parse plugin config: %w", err)
	}
	// A plugin in a list inherits the list's name and version.
	if list.Name != "" {
		conf["name"] = list.Name
	}
	if list.CNIVersion != "" {
		conf["cniVersion"] = list.CNIVersion
	}
	if prev != nil {
		conf["prevResult"] = prev
	}
	typ, _ := conf["type"].(string)
	bin, err := findPlugin(typ, inv.Path)
	if err != nil {
		return nil, err
	}
	stdin, err := json.Marshal(conf)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(bin)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(),
		"CNI_COMMAND="+inv.Command, "CNI_CONTAINERID="+inv.ContainerID, "CNI_NETNS="+inv.NetNS,
		"CNI_IFNAME="+inv.IfName, "CNI_PATH="+inv.Path)
	runErr := cmd.Run()
	if runErr != nil {
		var perr Error
		if json.Unmarshal(stdout.Bytes(), &perr) == nil && perr.Msg != "" {
			return nil, &perr
		}
		return nil, fmt.Errorf("cni: %s %s: %w: %s", typ, inv.Command, runErr, strings.TrimSpace(stderr.String()))
	}
	if inv.Command != "ADD" || stdout.Len() == 0 {
		return nil, nil
	}
	var res Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("cni: %s returned an unreadable result: %w", typ, err)
	}
	return &res, nil
}

func findPlugin(typ, path string) (string, error) {
	if typ == "" || strings.ContainsAny(typ, `/\`) {
		return "", fmt.Errorf("cni: invalid plugin type %q", typ)
	}
	for _, dir := range filepath.SplitList(path) {
		bin := filepath.Join(dir, typ)
		if st, err := os.Stat(bin); err == nil && !st.IsDir() {
			return bin, nil
		}
	}
	return "", errors.New("cni: plugin " + typ + " not found in " + path)
}
