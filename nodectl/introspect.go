package nodectl

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

const introUsage = `
  get      <node> <kind> [arg]      image boot memory psi mounts sysctl modules processes netstat cgroups services tailscale kubelet time config
  ps       <node> [rss|cpu] [-n N]  processes by memory or CPU
  netstat  <node>                   listening sockets
  dmesg    <node> [-f]              kernel log
  events   <node> [-f]              unit state changes, boots, config changes (journal, PID 1)
  service  <node> status|start|stop|restart <unit>      allowlisted units only
  ls       <node> <path>            list an allowlisted directory (/var/log /run/edge /var/lib/edge /etc/rancher)
  read     <node> <path> [-n bytes] read an allowlisted file (never secrets); the tail if it is larger
  whoami   <node>                   your role on the node (from the Tailscale grant)
  audit    <node>                   who called what (the noded audit log)
  health   <node>|--all             the boot checks plus listeners and memory pressure
  support  <node>... [-o dir]       diagnostics bundle (tar.gz, encrypted with age if available)
  pcap     <node> [--iface I] [--host IP] [--port N] [--proto tcp|udp|icmp] [--seconds 30] [--max-bytes N] > file.pcap
  dashboard                         all nodes, refreshed (needs kubectl)
  completion bash|zsh               shell completion
`

func printJSON(ctx context.Context, cl pb.NodedClient, kind, arg string, limit uint32) error {
	r, err := cl.Inspect(ctx, &pb.InspectRequest{Kind: kind, Arg: arg, Limit: limit})
	if err != nil {
		return err
	}
	fmt.Println(r.Json)
	return nil
}

func introspect(ctx context.Context, cmd, node string, cl pb.NodedClient, rest []string) (bool, error) {
	switch cmd {
	case "get":
		if err := need(rest, 1, "get <node> <kind> [arg]"); err != nil {
			return true, err
		}
		arg := ""
		if len(rest) > 1 {
			arg = rest[1]
		}
		return true, printJSON(ctx, cl, rest[0], arg, 0)
	case "ps":
		fs := flag.NewFlagSet("ps", flag.ContinueOnError)
		n := fs.Uint("n", 20, "how many")
		sortBy := "rss"
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			sortBy, rest = rest[0], rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return true, err
		}
		return true, printJSON(ctx, cl, "processes", sortBy, uint32(*n))
	case "netstat":
		return true, printJSON(ctx, cl, "netstat", "", 0)
	case "dmesg":
		return true, logs(ctx, cl, append([]string{"kernel"}, rest...))
	case "events":
		fs := flag.NewFlagSet("events", flag.ContinueOnError)
		f := fs.Bool("f", false, "follow")
		n := fs.Uint("n", 100, "lines")
		if err := fs.Parse(rest); err != nil {
			return true, err
		}
		st, err := cl.Events(ctx, &pb.EventsRequest{TailLines: uint32(*n), Follow: *f})
		if err != nil {
			return true, err
		}
		for {
			l, err := st.Recv()
			if err == io.EOF {
				return true, nil
			}
			if err != nil {
				return true, err
			}
			fmt.Println(l.Text)
		}
	case "service":
		if err := need(rest, 2, "service <node> status|start|stop|restart <unit>"); err != nil {
			return true, err
		}
		r, err := cl.Service(ctx, &pb.ServiceRequest{Action: rest[0], Unit: rest[1]})
		if err != nil {
			return true, err
		}
		fmt.Print(r.Output)
		return true, nil
	case "ls":
		if err := need(rest, 1, "ls <node> <path>"); err != nil {
			return true, err
		}
		r, err := cl.ListFiles(ctx, &pb.ListFilesRequest{Path: rest[0]})
		if err != nil {
			return true, err
		}
		fmt.Println(r.Json)
		return true, nil
	case "read":
		if err := need(rest, 1, "read <node> <path> [-n bytes]"); err != nil {
			return true, err
		}
		fs := flag.NewFlagSet("read", flag.ContinueOnError)
		n := fs.Uint("n", 0, "max bytes (the tail is returned)")
		if err := fs.Parse(rest[1:]); err != nil {
			return true, err
		}
		r, err := cl.ReadFile(ctx, &pb.ReadFileRequest{Path: rest[0], MaxBytes: uint32(*n)})
		if err != nil {
			return true, err
		}
		_, err = os.Stdout.Write(r.Data)
		return true, err
	case "whoami":
		r, err := cl.WhoAmI(ctx, &pb.WhoAmIRequest{})
		if err != nil {
			return true, err
		}
		fmt.Printf("device %s\nuser   %s\nrole   %s (roles are %s on %s)\n", r.Device, r.User, r.Role, r.Mode, node)
		return true, nil
	case "audit":
		st, err := cl.Logs(ctx, &pb.LogsRequest{Unit: "noded.service", TailLines: 2000})
		if err != nil {
			return true, err
		}
		for {
			l, err := st.Recv()
			if err == io.EOF {
				return true, nil
			}
			if err != nil {
				return true, err
			}
			if strings.Contains(l.Text, " audit id=") {
				fmt.Println(l.Text)
			}
		}
	case "health":
		return true, health(ctx, node, cl)
	case "pcap":
		return true, pcap(ctx, cl, rest)
	}
	return false, nil
}

func health(ctx context.Context, node string, cl pb.NodedClient) error {
	r, err := cl.Health(ctx, &pb.HealthRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("%s: ok=%v\n%s\n", node, r.Ok, r.Json)
	if !r.Ok {
		return fmt.Errorf("%s is not healthy", node)
	}
	return nil
}

func pcap(ctx context.Context, cl pb.NodedClient, rest []string) error {
	fs := flag.NewFlagSet("pcap", flag.ContinueOnError)
	iface := fs.String("iface", "any", "interface")
	host := fs.String("host", "", "only this IP (either direction)")
	port := fs.Uint("port", 0, "only this port")
	proto := fs.String("proto", "", "tcp, udp or icmp")
	secs := fs.Uint("seconds", 30, "duration (max 120)")
	max := fs.Uint("max-bytes", 20<<20, "size cap (max 50 MB)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if isTerminal(os.Stdout) {
		return fmt.Errorf("pcap writes binary data: redirect it, for example `> capture.pcap`")
	}
	st, err := cl.Pcap(ctx, &pb.PcapRequest{Iface: *iface, Host: *host, Port: uint32(*port), Proto: *proto, Seconds: uint32(*secs), MaxBytes: uint32(*max)})
	if err != nil {
		return err
	}
	return copyChunks(os.Stdout, func() ([]byte, error) {
		c, err := st.Recv()
		if err != nil {
			return nil, err
		}
		return c.Data, nil
	})
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func copyChunks(w io.Writer, next func() ([]byte, error)) error {
	for {
		b, err := next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
	}
}

// support collects the bundle of each node into dir, encrypting with age (to ~/.ssh/id_ed25519.pub or $NODECTL_AGE_RECIPIENTS)
// when the age binary is present; otherwise the file is left unencrypted and a warning says so.
func (e *env) support(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("support", flag.ContinueOnError)
	dir := fs.String("o", ".", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	nodes := fs.Args()
	if len(nodes) == 0 {
		return fmt.Errorf("support <node>... [-o dir]")
	}
	for _, n := range nodes {
		cl, closeFn, err := e.dial(ctx, n)
		if err != nil {
			return err
		}
		st, err := cl.Support(ctx, &pb.SupportRequest{})
		if err != nil {
			closeFn()
			return err
		}
		var buf bytes.Buffer
		err = copyChunks(&buf, func() ([]byte, error) {
			c, err := st.Recv()
			if err != nil {
				return nil, err
			}
			return c.Data, nil
		})
		closeFn()
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		name := filepath.Join(*dir, fmt.Sprintf("support-%s-%s.tar.gz", n, time.Now().Format("20060102-150405")))
		if err := writeMaybeEncrypted(name, buf.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func writeMaybeEncrypted(name string, data []byte) error {
	recip := os.Getenv("NODECTL_AGE_RECIPIENTS")
	if recip == "" {
		if h, err := os.UserHomeDir(); err == nil {
			recip = filepath.Join(h, ".ssh", "id_ed25519.pub")
		}
	}
	if _, err := exec.LookPath("age"); err == nil {
		if _, err := os.Stat(recip); err == nil {
			c := exec.Command("age", "-R", recip, "-o", name+".age")
			c.Stdin = bytes.NewReader(data)
			c.Stderr = os.Stderr
			if err := c.Run(); err != nil {
				return err
			}
			fmt.Println("wrote", name+".age (age-encrypted)")
			return nil
		}
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "warning: age or a recipient file was not found; the bundle is NOT encrypted. It contains journals: handle accordingly.")
	fmt.Println("wrote", name)
	return nil
}

func nodeNames(ctx context.Context) ([]string, error) {
	out, err := kubectl(ctx, "get", "nodes", "-l", "node.vmd1.dev/image-version", "-o", "name")
	if err != nil || out == "" {
		out, err = kubectl(ctx, "get", "nodes", "-o", "name")
	}
	if err != nil {
		return nil, err
	}
	var n []string
	for _, l := range strings.Fields(out) {
		n = append(n, strings.TrimPrefix(l, "node/"))
	}
	sort.Strings(n)
	return n, nil
}

// fleetHealth runs Health on every node; it is the gate used before and between rollout steps.
func (e *env) fleetHealth(ctx context.Context) error {
	nodes, err := nodeNames(ctx)
	if err != nil {
		return err
	}
	var bad []string
	for _, n := range nodes {
		cl, closeFn, err := e.dial(ctx, n)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			r, herr := cl.Health(cctx, &pb.HealthRequest{})
			cancel()
			closeFn()
			err = herr
			if err == nil && !r.Ok {
				err = fmt.Errorf("unhealthy")
			}
		}
		if err != nil {
			bad = append(bad, n+" ("+err.Error()+")")
			fmt.Printf("%-10s FAIL %v\n", n, err)
			continue
		}
		fmt.Printf("%-10s ok\n", n)
	}
	if len(bad) > 0 {
		return fmt.Errorf("unhealthy: %s", strings.Join(bad, ", "))
	}
	return nil
}

func (e *env) dashboard(ctx context.Context) error {
	for {
		nodes, err := nodeNames(ctx)
		if err != nil {
			return err
		}
		fmt.Print("\033[H\033[2J")
		fmt.Printf("%-10s %-10s %-9s %-8s %-9s %-7s %s\n", "NODE", "IMAGE", "BOOT", "MEM-AV", "PSI-MEM", "UPTIME", "UNITS (non-active)")
		for _, n := range nodes {
			cl, closeFn, err := e.dial(ctx, n)
			if err != nil {
				fmt.Printf("%-10s %v\n", n, err)
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			s, err := cl.Status(cctx, &pb.StatusRequest{})
			cancel()
			closeFn()
			if err != nil {
				fmt.Printf("%-10s unreachable: %v\n", n, err)
				continue
			}
			var down []string
			for u, st := range s.Units {
				if st != "active" {
					down = append(down, u+"="+st)
				}
			}
			sort.Strings(down)
			fmt.Printf("%-10s %-10s %-9s %-8s %-9.2f %-7s %s\n", n, s.ImageVersion, s.BootAssessment, fmt.Sprintf("%dMiB", s.MemAvailableBytes>>20), s.MemPressureSomeAvg10, (time.Duration(s.UptimeSeconds) * time.Second).Round(time.Minute), strings.Join(down, " "))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

const verbs = "list status logs check stage update rollback reboot registry channel config get ps netstat dmesg events service ls read whoami audit health support pcap dashboard verify completion debug"

func completion(shell string) error {
	switch shell {
	case "bash":
		fmt.Printf("_nodectl() { local cur=${COMP_WORDS[COMP_CWORD]}; if [ $COMP_CWORD -eq 1 ]; then COMPREPLY=($(compgen -W %q -- \"$cur\")); else COMPREPLY=($(compgen -W \"$(kubectl get nodes -o name 2>/dev/null | sed s,node/,,)\" -- \"$cur\")); fi; }\ncomplete -F _nodectl nodectl\n", verbs)
	case "zsh":
		fmt.Printf("#compdef nodectl\n_nodectl() { if (( CURRENT == 2 )); then compadd %s; else compadd ${(f)\"$(kubectl get nodes -o name 2>/dev/null | sed s,node/,,)\"}; fi }\ncompdef _nodectl nodectl\n", verbs)
	default:
		return fmt.Errorf("completion bash|zsh")
	}
	return nil
}
