package config

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// SysHost is the real Host: it writes the drop-ins and drives systemd.
type SysHost struct {
	Root string // "" in production; a temp dir in tests
	Kube *Kube
	Run  func(ctx context.Context, name string, args ...string) error
}

func (h *SysHost) path(p string) string { return filepath.Join(h.Root, p) }

var generated = []string{K3sDropin, SysctlDropin, TimesyncConf, LockdownFlag}

func (h *SysHost) Write(r Rendered) error {
	for _, p := range generated {
		if _, ok := r[p]; !ok {
			if err := os.Remove(h.path(p)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	for p, c := range r {
		if err := os.MkdirAll(filepath.Dir(h.path(p)), 0o755); err != nil {
			return err
		}
		tmp := h.path(p) + ".tmp"
		if err := os.WriteFile(tmp, []byte(c), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, h.path(p)); err != nil {
			return err
		}
	}
	return nil
}

func (h *SysHost) run(ctx context.Context, name string, args ...string) error {
	if h.Run != nil {
		return h.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %.200s", name, args, err, out)
	}
	return nil
}

func (h *SysHost) Apply(im Impact) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if im.ReloadSysctl {
		if err := h.run(ctx, "/usr/lib/systemd/systemd-sysctl"); err != nil {
			return err
		}
	}
	if im.RestartTimesync {
		if err := h.run(ctx, "systemctl", "restart", "systemd-timesyncd.service"); err != nil {
			return err
		}
	}
	if im.RestartK3s {
		return h.run(ctx, "systemctl", "restart", "k3s-agent.service")
	}
	return nil
}

func (h *SysHost) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	time.Sleep(5 * time.Second) // let the restart take the old Ready condition away
	var last error
	for {
		if err := h.run(ctx, "systemctl", "is-active", "--quiet", "k3s-agent.service"); err != nil {
			last = fmt.Errorf("k3s-agent not active")
		} else if _, _, ready, err := h.Kube.Node(ctx); err != nil {
			last = err
		} else if !ready {
			last = fmt.Errorf("node not Ready")
		} else {
			return nil
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(5 * time.Second):
		}
	}
}

// Loop reconciles on an interval until ctx ends. It applies one node at a time (a node waits while another reports
// "applying") with a per-node jitter so a bad object cannot hit every node together.
func Loop(ctx context.Context, k *Kube, a *Agent, ov *Overrides, every time.Duration) {
	if err := a.Restore(); err != nil {
		log.Printf("config: restore last-known-good: %v", err)
	}
	jit := fnv.New32a()
	jit.Write([]byte(k.NodeName))
	rng := rand.New(rand.NewSource(int64(jit.Sum32())))
	first := true
	for {
		a.AfterBoot = first
		one(ctx, k, a, ov, time.Duration(rng.Int63n(int64(30*time.Second))))
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func one(ctx context.Context, k *Kube, a *Agent, ov *Overrides, jitter time.Duration) {
	labels, _, _, err := k.Node(ctx)
	if err != nil {
		log.Printf("config: node: %v (keeping the running config)", err)
		return
	}
	objs, err := k.Configs(ctx)
	if err != nil {
		log.Printf("config: list EdgeNodeConfig: %v (keeping the running config)", err)
		return
	}
	eff := Merge(objs, labels)
	if o := ov.Active(); o != nil {
		Overlay(&eff, *o)
	}
	if a.Store.Load().AppliedHash == eff.Hash() || (eff.IsZero() && a.Store.Load().Applied == nil) {
		_ = k.Report(ctx, StateApplied, eff.Hash(), "")
		return
	}
	// a change is about to be applied: wait our turn, then announce it
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}
	for i := 0; i < 20; i++ {
		busy, err := k.OthersApplying(ctx)
		if err != nil || !busy {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	_ = k.Report(ctx, "applying", eff.Hash(), "")
	res := a.Reconcile(ctx, eff)
	log.Printf("config: %s hash=%s %s", res.State, res.Hash, res.Error)
	if err := k.Report(ctx, res.State, res.Hash, res.Error); err != nil {
		log.Printf("config: report: %v", err)
	}
}
