package nodectl

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

func region(node string) string {
	if i := strings.IndexByte(node, '-'); i > 0 {
		return node[:i]
	}
	return node
}

// rolloutOrder puts the canary first, then walks the regions in turn so that no two consecutive nodes share a region
// (a region never loses both of its nodes back to back).
func rolloutOrder(nodes []string, canary string) []string {
	byRegion := map[string][]string{}
	for _, n := range nodes {
		if n != canary {
			byRegion[region(n)] = append(byRegion[region(n)], n)
		}
	}
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	var order []string
	for _, n := range nodes {
		if n == canary {
			order = append(order, n)
		}
	}
	last := region(canary)
	if len(order) == 0 {
		last = ""
	}
	for {
		picked := ""
		// prefer the region with the most nodes left that differs from the previous one
		best := 0
		for _, r := range regions {
			if r != last && len(byRegion[r]) > best {
				picked, best = r, len(byRegion[r])
			}
		}
		if picked == "" { // only the previous region has nodes left
			for _, r := range regions {
				if len(byRegion[r]) > 0 {
					picked = r
				}
			}
		}
		if picked == "" {
			return order
		}
		order = append(order, byRegion[picked][0])
		byRegion[picked] = byRegion[picked][1:]
		last = picked
	}
}

func (e *env) rollout(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rollout", flag.ContinueOnError)
	canary := fs.String("canary", "hera-02", "node that goes first")
	settle := fs.Duration("settle", 10*time.Minute, "wait after each node before the health gate and the next node")
	from := fs.String("from", "", "start at this node (earlier nodes in the order are skipped)")
	cfg := fs.Bool("config", false, "watch a config revision instead: wait until every node reports config-state applied")
	resume := fs.Bool("resume", true, "skip nodes already running the target version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	nodes, err := nodeNames(ctx)
	if err != nil {
		return err
	}
	if *cfg {
		return e.configRollout(ctx, nodes, *settle)
	}
	version := ""
	if fs.NArg() > 0 {
		version = fs.Arg(0)
	}
	order := rolloutOrder(nodes, *canary)
	if *from != "" {
		i := indexOf(order, *from)
		if i < 0 {
			return fmt.Errorf("--from %s: not a node of this fleet (%s)", *from, strings.Join(order, " "))
		}
		order = order[i:]
	}
	target := version
	if target == "" {
		cl, closeFn, err := e.dial(ctx, order[0])
		if err != nil {
			return err
		}
		r, err := cl.CheckUpdate(ctx, &pb.CheckUpdateRequest{})
		closeFn()
		if err != nil {
			return err
		}
		target = r.NewestVersion
		if target == "" {
			fmt.Printf("fleet is up to date (%s on %s)\n", r.CurrentVersion, order[0])
			return nil
		}
	}
	fmt.Printf("rollout to %s, order: %s (settle %s, health gate between nodes)\n", target, strings.Join(order, " > "), *settle)
	if !e.yes {
		fmt.Println("dry run: re-run with --yes to do it")
		return nil
	}
	fmt.Println("pre-check: fleet health")
	if err := e.fleetHealth(ctx); err != nil {
		return fmt.Errorf("refusing to start on an unhealthy fleet: %w", err)
	}
	for i, n := range order {
		cl, closeFn, err := e.dial(ctx, n)
		if err != nil {
			return fmt.Errorf("%s: %w (stopped before touching it; resume with --from %s)", n, err, n)
		}
		s, err := cl.Status(ctx, &pb.StatusRequest{})
		if err != nil {
			closeFn()
			return fmt.Errorf("%s: %w", n, err)
		}
		if *resume && s.ImageVersion == target {
			fmt.Printf("%s already runs %s, skipping\n", n, target)
			closeFn()
			continue
		}
		fmt.Printf("\n== [%d/%d] %s: %s -> %s\n", i+1, len(order), n, s.ImageVersion, target)
		err = update(ctx, e, cl, n, target)
		closeFn()
		if err != nil {
			return fmt.Errorf("%s failed: %w\nrollout stopped; the remaining nodes were not touched (resume with: nodectl --yes rollout --from %s %s)", n, err, n, target)
		}
		if i == len(order)-1 {
			break
		}
		fmt.Printf("settling %s, then the health gate\n", *settle)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*settle):
		}
		if err := e.fleetHealth(ctx); err != nil {
			return fmt.Errorf("health gate failed after %s: %w\nrollout stopped; consider `nodectl --yes rollback %s <previous version>`", n, err, n)
		}
	}
	fmt.Println("\nrollout complete")
	return nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// configRollout waits until every node reports config-state applied (the nodes stagger themselves; this is the gate).
func (e *env) configRollout(ctx context.Context, nodes []string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		pending := 0
		for _, n := range nodes {
			st, _ := kubectl(ctx, "get", "node", n, "-o", `jsonpath={.metadata.annotations.node\.vmd1\.dev/config-state}`)
			msg, _ := kubectl(ctx, "get", "node", n, "-o", `jsonpath={.metadata.annotations.node\.vmd1\.dev/config-error}`)
			switch st {
			case "rejected", "rolled-back":
				return fmt.Errorf("%s reports config-state %s: %s", n, st, msg)
			case "applied":
			default:
				pending++
			}
		}
		if pending == 0 {
			fmt.Println("every node reports config-state applied")
			return e.fleetHealth(ctx)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d node(s) have not applied the config within %s", pending, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func nodeReady(ctx context.Context, node string) bool {
	out, _ := kubectl(ctx, "get", "node", node, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
	return out == "True"
}

// otherReadyInRegion counts other Ready, schedulable nodes of the same region.
func otherReadyInRegion(ctx context.Context, node string) int {
	names, err := nodeNames(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, o := range names {
		if o != node && region(o) == region(node) && nodeReady(ctx, o) {
			if u, _ := kubectl(ctx, "get", "node", o, "-o", "jsonpath={.spec.unschedulable}"); u != "true" {
				n++
			}
		}
	}
	return n
}

func (e *env) reset(ctx context.Context, node string, cl pb.NodedClient, rest []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	wipe := fs.String("wipe", "k3s", "k3s (keep Tailscale identity and node password) or all-var (factory reset)")
	keepNode := fs.Bool("keep-node-object", false, "all-var: do not delete the Node object and node password secret first")
	force := fs.Bool("force", false, "allow it even if this is the last Ready node of its region")
	reboot := fs.Bool("reboot", true, "false: stop after the wipe, for inspection")
	confirm := fs.String("confirm", "", "all-var: type the node name again")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *wipe != "k3s" && *wipe != "all-var" {
		return fmt.Errorf("--wipe must be k3s or all-var")
	}
	fmt.Printf("plan for %s (--wipe %s): %s", node, *wipe, kubePlan(e))
	fmt.Printf("stop k3s-agent, wipe %s, %s\n", map[string]string{"k3s": "/var/lib/rancher (except the node password), /var/lib/kubelet, /var/lib/cni", "all-var": "everything under /var (k3s, Tailscale state, overrides, logs, node password)"}[*wipe],
		map[bool]string{true: "reboot, wait for it to re-provision from instance metadata and come back Ready", false: "no reboot (k3s stays stopped)"}[*reboot || *wipe == "all-var"])
	if *wipe == "all-var" && !*keepNode {
		fmt.Printf("first: delete Node %s and secret %s.node-password.k3s (required: the node password is gone). After it returns, the old Tailscale device is still listed: delete it in the admin console, or the node joins as %s-1.\n", node, node, node)
	}
	if !e.yes {
		fmt.Println("re-run with --yes to do it")
		return nil
	}
	if *wipe == "all-var" && *confirm != node {
		return fmt.Errorf("an all-var reset needs --confirm %s", node)
	}
	if !e.noKube && !*force && otherReadyInRegion(ctx, node) == 0 {
		return fmt.Errorf("%s is the last Ready node of region %q; refusing (use --force)", node, region(node))
	}
	pf, err := cl.ResetPreflight(ctx, &pb.ResetRequest{Wipe: *wipe})
	if err != nil {
		return err
	}
	if !pf.Ok {
		return fmt.Errorf("pre-flight failed, nothing was changed: %s", pf.Detail)
	}
	fmt.Println("pre-flight:", pf.Detail)
	before, err := cl.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		return err
	}
	if !e.noKube {
		if out, err := kubectl(ctx, "cordon", node); err != nil {
			return fmt.Errorf("cordon: %v %s", err, out)
		}
		if out, err := kubectl(ctx, "drain", node, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=180s"); err != nil {
			return fmt.Errorf("drain failed (node stays cordoned, nothing wiped): %v\n%s", err, out)
		}
		if *wipe == "all-var" && !*keepNode {
			_, _ = kubectl(ctx, "delete", "node", node, "--wait=false")
			_, _ = kubectl(ctx, "-n", "kube-system", "delete", "secret", node+".node-password.k3s", "--ignore-not-found")
		}
	}
	r, err := cl.Reset(ctx, &pb.ResetRequest{Wipe: *wipe, Reboot: *reboot})
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	fmt.Printf("wiped %d entries\n", len(r.Removed))
	if !r.Rebooting {
		fmt.Printf("%s is stopped and wiped; reboot it (nodectl reboot) or restart k3s-agent when done\n", node)
		return nil
	}
	if err := waitBoot(ctx, cl, before.BootId, 15*time.Minute); err != nil {
		return fmt.Errorf("%s: %w", node, err)
	}
	fmt.Printf("%s is back (image %s)\n", node, mustVersion(ctx, cl))
	if !e.noKube {
		for i := 0; i < 120; i++ {
			if nodeReady(ctx, node) {
				_, _ = kubectl(ctx, "uncordon", node)
				fmt.Printf("%s is Ready and schedulable again\n", node)
				return nil
			}
			time.Sleep(5 * time.Second)
		}
		return fmt.Errorf("%s did not become Ready within 10 minutes of re-provisioning (it stays cordoned)", node)
	}
	return nil
}

func mustVersion(ctx context.Context, cl pb.NodedClient) string {
	if s, err := cl.Status(ctx, &pb.StatusRequest{}); err == nil {
		return s.ImageVersion
	}
	return "?"
}

func waitBoot(ctx context.Context, cl pb.NodedClient, oldBoot string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	time.Sleep(10 * time.Second)
	for {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := cl.Status(sctx, &pb.StatusRequest{})
		cancel()
		if err == nil && s.BootId != "" && s.BootId != oldBoot {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("did not come back within %s; check the console", timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// hardReset files a platform-neutral request on the Node object. A platform adapter in the cluster (today
// oci-reboot-controller) does the actual power cycle, clears the annotation and records an event. nodectl never calls a
// cloud API.
func (e *env) hardReset(ctx context.Context, node string, rest []string) error {
	fs := flag.NewFlagSet("hard-reset", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the node to return")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if !e.yes {
		fmt.Printf("plan: annotate node %s with node.vmd1.dev/hard-reset-requested, wait up to %s for the platform adapter to power cycle it and for noded to answer again\nre-run with --yes to do it (this is an unclean reset, use it only when noded is unreachable)\n", node, *timeout)
		return nil
	}
	var oldBoot string
	if cl, closeFn, err := e.dial(ctx, node); err == nil {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if s, err := cl.Status(sctx, &pb.StatusRequest{}); err == nil {
			oldBoot = s.BootId
		}
		cancel()
		closeFn()
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if out, err := kubectl(ctx, "annotate", "node", node, "node.vmd1.dev/hard-reset-requested="+ts, "--overwrite"); err != nil {
		return fmt.Errorf("annotate: %v %s", err, out)
	}
	fmt.Printf("requested at %s; waiting for the platform adapter\n", ts)
	deadline := time.Now().Add(*timeout)
	handled := false
	for time.Now().Before(deadline) {
		ann, _ := kubectl(ctx, "get", "node", node, "-o", `jsonpath={.metadata.annotations.node\.vmd1\.dev/hard-reset-requested}`)
		if ann == "" {
			handled = true
		}
		if handled {
			if cl, closeFn, err := e.dial(ctx, node); err == nil {
				sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				s, serr := cl.Status(sctx, &pb.StatusRequest{})
				cancel()
				closeFn()
				if serr == nil && (oldBoot == "" || s.BootId != oldBoot) {
					fmt.Printf("%s is back (boot %s)\n", node, s.BootId)
					return nil
				}
			}
		} else if time.Since(deadline.Add(-*timeout)) > 3*time.Minute {
			return fmt.Errorf("no platform adapter handled the request within 3 minutes (the annotation is still set). Is oci-reboot-controller running and does it support hard-reset? Remove it with: kubectl annotate node %s node.vmd1.dev/hard-reset-requested-", node)
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("%s did not answer again within %s", node, *timeout)
}
