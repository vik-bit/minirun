// Command minicni is a CNI plugin (when run by a container runtime) and the node
// daemon that sets it up in Kubernetes (`minicni daemon`).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vik-bit/minirun/internal/cni"
	"github.com/vik-bit/minirun/internal/daemon"
	"github.com/vik-bit/minirun/internal/plugin"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		runDaemon(os.Args[2:])
		return
	}
	os.Exit(runPlugin())
}

// runPlugin follows the CNI contract: the command comes from CNI_COMMAND, the
// network config from stdin; a result or a JSON error goes to stdout.
func runPlugin() int {
	a := plugin.Args{
		Command: os.Getenv("CNI_COMMAND"), ContainerID: os.Getenv("CNI_CONTAINERID"),
		NetNS: os.Getenv("CNI_NETNS"), IfName: os.Getenv("CNI_IFNAME"),
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(&cni.Error{Code: cni.ErrIOFailure, Msg: "cannot read stdin", Details: err.Error()})
	}

	switch a.Command {
	case "VERSION":
		return emit(map[string]any{"cniVersion": cni.Version, "supportedVersions": cni.SupportedVersions})
	case "ADD", "DEL", "CHECK":
	default:
		return fail(&cni.Error{Code: cni.ErrInvalidEnvironment, Msg: "unknown CNI_COMMAND " + a.Command})
	}
	if a.ContainerID == "" || a.IfName == "" || (a.Command != "DEL" && a.NetNS == "") {
		return fail(&cni.Error{Code: cni.ErrInvalidEnvironment, Msg: "CNI_CONTAINERID, CNI_IFNAME and CNI_NETNS are required"})
	}
	var version struct {
		CNIVersion string `json:"cniVersion"`
	}
	if err := json.Unmarshal(stdin, &version); err == nil && version.CNIVersion != cni.Version {
		return fail(&cni.Error{Code: cni.ErrIncompatibleVersion, Msg: "unsupported cniVersion " + version.CNIVersion})
	}

	switch a.Command {
	case "ADD":
		res, err := plugin.Add(a, stdin)
		if err != nil {
			return fail(err)
		}
		return emit(res)
	case "DEL":
		if err := plugin.Del(a, stdin); err != nil {
			return fail(err)
		}
	case "CHECK":
		if err := plugin.Check(a, stdin); err != nil {
			return fail(err)
		}
	}
	return 0
}

func emit(v any) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		return 1
	}
	return 0
}

func fail(err error) int {
	var e *cni.Error
	if !errors.As(err, &e) {
		e = &cni.Error{Code: 100, Msg: err.Error()}
	}
	e.CNIVersion = cni.Version
	_ = json.NewEncoder(os.Stdout).Encode(e)
	fmt.Fprintln(os.Stderr, "minicni:", e.Error())
	return 1
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfg := daemon.Config{}
	fs.StringVar(&cfg.ClusterCIDR, "cluster-cidr", "10.244.0.0/16", "CIDR covering every node's pod subnet")
	fs.StringVar(&cfg.BinDir, "cni-bin-dir", "/host/opt/cni/bin", "where to install the plugin")
	fs.StringVar(&cfg.ConfDir, "cni-conf-dir", "/host/etc/cni/net.d", "where to write the CNI config")
	fs.DurationVar(&cfg.Interval, "interval", 10*time.Second, "how often to re-sync routes")
	_ = fs.Parse(args)
	cfg.NodeName = os.Getenv("NODE_NAME")
	if cfg.NodeName == "" {
		log.Fatal("NODE_NAME must be set (use the downward API)")
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	cfg.PluginSrc = self

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
