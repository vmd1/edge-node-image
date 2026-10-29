// nodectl is the client for noded: status, logs, update, rollback, reboot, registry mirror and update channel.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

const usage = `usage: nodectl [--addr host:port] [--no-kube] [--yes] <command> [args]

  list                              nodes with Ready state and image version (needs kubectl)
  status   <node>                   image version, boot entry, boot assessment, pressure, units
  logs     <node> <unit> [-n N] [-f]
  check    <node>                   is a newer image available?
  stage    <node> [version]         download an update into the inactive slot (does not reboot)
  update   <node> [version]         stage, cordon+drain, reboot, wait, uncordon      (needs --yes)
  rollback <node> <version>         boot an installed version once, reboot, wait      (needs --yes)
  reboot   <node>                   drain, reboot, wait, uncordon                     (needs --yes)
  registry <node> show|set [--mirror URL] [--fallback URL]
  channel  <node> show|set <url>|reset   update source (https release directory)
  debug    <node>                   kubectl debug node/<node>

<node> is a Kubernetes node name (address looked up with kubectl), or use --addr to talk to one agent directly.
`

type env struct {
	addr   string
	noKube bool
	yes    bool
	port   string
}

func main() {
	e := &env{}
	fs := flag.NewFlagSet("nodectl", flag.ExitOnError)
	fs.StringVar(&e.addr, "addr", "", "agent address host:port (skips the kubectl lookup)")
	fs.BoolVar(&e.noKube, "no-kube", false, "do not touch Kubernetes (no drain, cordon or wait for Ready)")
	fs.BoolVar(&e.yes, "yes", false, "actually do it")
	fs.StringVar(&e.port, "port", "7777", "agent port")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	if err := run(context.Background(), e, args); err != nil {
		fmt.Fprintln(os.Stderr, "nodectl:", err)
		os.Exit(1)
	}
}

func kubectl(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (e *env) dial(ctx context.Context, node string) (pb.NodedClient, func(), error) {
	addr := e.addr
	if addr == "" {
		ip, err := kubectl(ctx, "get", "node", node, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
		if err != nil || ip == "" {
			return nil, nil, fmt.Errorf("cannot find the address of node %q: %v %s", node, err, ip)
		}
		addr = ip + ":" + e.port
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, err
	}
	return pb.NewNodedClient(conn), func() { conn.Close() }, nil
}

func need(args []string, n int, what string) error {
	if len(args) < n {
		return fmt.Errorf("%s", what)
	}
	return nil
}

func run(ctx context.Context, e *env, args []string) error {
	cmd, rest := args[0], args[1:]
	if cmd == "list" {
		return list(ctx)
	}
	if cmd == "debug" {
		if err := need(rest, 1, "debug <node>"); err != nil {
			return err
		}
		c := exec.CommandContext(ctx, "kubectl", "debug", "node/"+rest[0], "-it", "--image=debian:trixie-slim")
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	if err := need(rest, 1, cmd+" needs a node"); err != nil {
		return err
	}
	node, rest := rest[0], rest[1:]
	cl, closeFn, err := e.dial(ctx, node)
	if err != nil {
		return err
	}
	defer closeFn()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	switch cmd {
	case "status":
		return status(cctx, cl)
	case "logs":
		return logs(cctx, cl, rest)
	case "check":
		r, err := cl.CheckUpdate(cctx, &pb.CheckUpdateRequest{})
		if err != nil {
			return err
		}
		if r.NewestVersion == "" {
			fmt.Printf("%s is up to date (%s)\n", node, r.CurrentVersion)
		} else {
			fmt.Printf("%s: %s -> %s available\n", node, r.CurrentVersion, r.NewestVersion)
		}
		return nil
	case "stage":
		v := ""
		if len(rest) > 0 {
			v = rest[0]
		}
		r, err := cl.StageUpdate(cctx, &pb.StageUpdateRequest{TargetVersion: v})
		if err != nil {
			return err
		}
		fmt.Print(r.Output)
		fmt.Printf("staged %q on %s; it boots on the next reboot (use `update` or `reboot`)\n", r.StagedVersion, node)
		return nil
	case "update":
		v := ""
		if len(rest) > 0 {
			v = rest[0]
		}
		return update(cctx, e, cl, node, v)
	case "rollback":
		if err := need(rest, 1, "rollback <node> <version>"); err != nil {
			return err
		}
		return rollback(cctx, e, cl, node, rest[0])
	case "reboot":
		return cycle(cctx, e, cl, node, "reboot", func(ctx context.Context) error {
			_, err := cl.Reboot(ctx, &pb.RebootRequest{})
			return err
		})
	case "registry":
		return registry(cctx, cl, rest)
	case "channel":
		return channel(cctx, cl, rest)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func list(ctx context.Context) error {
	out, err := kubectl(ctx, "get", "nodes", "-o", `custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,UNSCHEDULABLE:.spec.unschedulable,IMAGE:.metadata.annotations.node\.vmd1\.dev/image-version`)
	fmt.Println(out)
	return err
}

func status(ctx context.Context, cl pb.NodedClient) error {
	s, err := cl.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("image version     %s\n", s.ImageVersion)
	fmt.Printf("booted entry      %s (%s)\n", s.BootedEntry, s.BootAssessment)
	if s.AvailableVersion != "" {
		fmt.Printf("update available  %s\n", s.AvailableVersion)
	}
	fmt.Printf("external ip       %s\n", s.ExternalIp)
	fmt.Printf("uptime            %ds\n", s.UptimeSeconds)
	fmt.Printf("memory available  %d MiB\n", s.MemAvailableBytes>>20)
	fmt.Printf("pressure (avg10)  memory %.2f  io %.2f\n", s.MemPressureSomeAvg10, s.IoPressureSomeAvg10)
	keys := make([]string, 0, len(s.Units))
	for k := range s.Units {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-28s %s\n", k, s.Units[k])
	}
	return nil
}

func logs(ctx context.Context, cl pb.NodedClient, rest []string) error {
	if err := need(rest, 1, "logs <node> <unit> [-n N] [-f]"); err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := fs.Uint("n", 100, "lines")
	f := fs.Bool("f", false, "follow")
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	st, err := cl.Logs(ctx, &pb.LogsRequest{Unit: rest[0], TailLines: uint32(*n), Follow: *f})
	if err != nil {
		return err
	}
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println(m.Text)
	}
}

// otherIngressProblems warns (never refuses) about other nodes of the same role that are not Ready or are cordoned.
func otherIngressProblems(ctx context.Context, self string) []string {
	out, err := kubectl(ctx, "get", "nodes", "-l", "node-role.kubernetes.io/ingress=true", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{" "}{.spec.unschedulable}{"\n"}{end}`)
	if err != nil {
		return []string{"could not list the other ingress nodes: " + out}
	}
	var p []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == self {
			continue
		}
		if f[1] != "True" {
			p = append(p, f[0]+" is not Ready")
		}
		if len(f) > 2 && f[2] == "true" {
			p = append(p, f[0]+" is cordoned")
		}
	}
	return p
}

func update(ctx context.Context, e *env, cl pb.NodedClient, node, version string) error {
	cur, err := cl.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return err
	}
	if !e.noKube {
		for _, w := range otherIngressProblems(ctx, node) {
			fmt.Println("WARNING:", w, "(continuing: this is a warning, not a lock)")
		}
	}
	if !e.yes {
		fmt.Printf("plan for %s (running %s): stage %s, %sreboot, wait for it to return\nre-run with --yes to do it\n",
			node, cur.ImageVersion, orNewest(version), kubePlan(e))
		return nil
	}
	r, err := cl.StageUpdate(ctx, &pb.StageUpdateRequest{TargetVersion: version})
	if err != nil {
		return err
	}
	fmt.Print(r.Output)
	target := r.StagedVersion
	err = cycle(ctx, e, cl, node, "update", func(ctx context.Context) error {
		_, err := cl.Reboot(ctx, &pb.RebootRequest{})
		return err
	})
	if err != nil {
		return err
	}
	now, err := cl.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return err
	}
	if target != "" && now.ImageVersion != target {
		return fmt.Errorf("%s came back on %s, not %s: the new image failed its health check and the node fell back (boot assessment: %s)", node, now.ImageVersion, target, now.BootAssessment)
	}
	fmt.Printf("%s is running %s (%s)\n", node, now.ImageVersion, now.BootAssessment)
	return nil
}

func orNewest(v string) string {
	if v == "" {
		return "the newest version"
	}
	return v
}

func kubePlan(e *env) string {
	if e.noKube {
		return ""
	}
	return "cordon+drain, "
}

func rollback(ctx context.Context, e *env, cl pb.NodedClient, node, version string) error {
	if !e.yes {
		fmt.Printf("plan for %s: boot installed version %s once, reboot, wait\nre-run with --yes to do it\n", node, version)
		return nil
	}
	if _, err := cl.SetBootSlot(ctx, &pb.SetBootSlotRequest{Version: version, OneShot: true}); err != nil {
		return err
	}
	return cycle(ctx, e, cl, node, "rollback", func(ctx context.Context) error {
		_, err := cl.Reboot(ctx, &pb.RebootRequest{})
		return err
	})
}

// cycle drains, reboots through fn, waits for the agent to return with a smaller uptime, then uncordons.
func cycle(ctx context.Context, e *env, cl pb.NodedClient, node, what string, reboot func(context.Context) error) error {
	if !e.yes {
		fmt.Printf("plan for %s: %sreboot, wait\nre-run with --yes to do it\n", node, kubePlan(e))
		return nil
	}
	before, err := cl.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return err
	}
	if !e.noKube {
		if out, err := kubectl(ctx, "cordon", node); err != nil {
			return fmt.Errorf("cordon: %v %s", err, out)
		}
		if out, err := kubectl(ctx, "drain", node, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=180s"); err != nil {
			return fmt.Errorf("drain failed (node stays cordoned): %v\n%s", err, out)
		}
	}
	fmt.Printf("%s: rebooting %s\n", what, node)
	if err := reboot(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Minute)
	time.Sleep(10 * time.Second)
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := cl.Status(sctx, &pb.StatusRequest{})
		cancel()
		if err == nil && s.BootId != "" && s.BootId != before.BootId {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not come back within 10 minutes; it stays cordoned. Check the OCI console", node)
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Printf("%s is back\n", node)
	if !e.noKube {
		for i := 0; i < 60; i++ {
			out, _ := kubectl(ctx, "get", "node", node, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
			if out == "True" {
				break
			}
			time.Sleep(5 * time.Second)
		}
		if out, err := kubectl(ctx, "uncordon", node); err != nil {
			return fmt.Errorf("uncordon: %v %s", err, out)
		}
	}
	return nil
}

func registry(ctx context.Context, cl pb.NodedClient, rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("registry <node> show|set [--mirror URL] [--fallback URL]")
	}
	switch rest[0] {
	case "show":
		r, err := cl.GetRegistryConfig(ctx, &pb.GetRegistryConfigRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("docker.io mirror   %q\ndocker.io fallback %q\n", r.DockerIoMirror, r.DockerIoFallback)
		return nil
	case "set":
		fs := flag.NewFlagSet("registry set", flag.ContinueOnError)
		m := fs.String("mirror", "", "mirror URL (empty = pull from Docker Hub directly)")
		f := fs.String("fallback", "https://registry-1.docker.io", "fallback URL")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		r, err := cl.SetRegistryConfig(ctx, &pb.RegistryConfig{DockerIoMirror: *m, DockerIoFallback: *f})
		if err != nil {
			return err
		}
		fmt.Printf("saved; k3s-agent restarted: %v\n", r.K3SAgentRestarted)
		return nil
	}
	return fmt.Errorf("registry <node> show|set")
}

func channel(ctx context.Context, cl pb.NodedClient, rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("channel <node> show|set <url>|reset")
	}
	switch rest[0] {
	case "show":
		c, err := cl.GetUpdateConfig(ctx, &pb.GetUpdateConfigRequest{})
		if err != nil {
			return err
		}
		if c.UpdateUrl == "" {
			fmt.Println("(image default)")
		} else {
			fmt.Println(c.UpdateUrl)
		}
		return nil
	case "set":
		if len(rest) < 2 {
			return fmt.Errorf("channel <node> set <url>")
		}
		_, err := cl.SetUpdateConfig(ctx, &pb.UpdateConfig{UpdateUrl: rest[1]})
		return err
	case "reset":
		_, err := cl.SetUpdateConfig(ctx, &pb.UpdateConfig{})
		return err
	}
	return fmt.Errorf("channel <node> show|set <url>|reset")
}
