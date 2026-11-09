package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config states reported on the Node object (annotation node.vmd1.dev/config-state).
const (
	StateApplied    = "applied"
	StateStaged     = "staged"
	StatePending    = "pending"
	StateRejected   = "rejected"
	StateRolledBack = "rolled-back"
)

// Host performs the side effects of an apply. The real implementation writes files and runs systemctl; tests use a fake.
type Host interface {
	// Write replaces the generated drop-ins with exactly r (files not in r that were generated before are removed).
	Write(r Rendered) error
	// Apply makes the impact take effect (restart k3s-agent, sysctl --system, restart timesyncd).
	Apply(im Impact) error
	// WaitHealthy returns nil once the node is Ready and the health checks pass, or an error after the timeout.
	WaitHealthy(ctx context.Context, timeout time.Duration) error
}

// Saved is the last-known-good state kept on /var, plus anything staged for the next boot.
type Saved struct {
	Applied     *Spec  `json:"applied,omitempty"`
	AppliedHash string `json:"appliedHash,omitempty"`
	Staged      *Spec  `json:"staged,omitempty"`
	StagedHash  string `json:"stagedHash,omitempty"`
}

type Store struct{ Path string }

func (s Store) Load() Saved {
	var v Saved
	if b, err := os.ReadFile(s.Path); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	return v
}

func (s Store) Save(v Saved) error {
	b, _ := json.MarshalIndent(v, "", " ")
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Result is what the agent reports.
type Result struct {
	State string
	Hash  string
	Error string
}

const (
	defaultTry  = 180 * time.Second
	defaultAuto = 300 * time.Second
)

// Agent applies effective specs one at a time.
type Agent struct {
	Host      Host
	Store     Store
	AfterBoot bool // true on the first reconcile after a boot: staged config applies now
	rendered  Rendered
}

// Reconcile moves the node toward the effective spec. It is idempotent: with nothing changed it returns the current state.
func (a *Agent) Reconcile(ctx context.Context, eff Spec) Result {
	hash := eff.Hash()
	saved := a.Store.Load()
	if err := eff.Validate(); err != nil {
		return Result{StateRejected, hash, err.Error()}
	}
	if saved.AppliedHash == hash && saved.Applied != nil || (saved.Applied == nil && eff.IsZero()) {
		return Result{StateApplied, hash, ""}
	}
	mode := eff.Mode
	if mode == "" {
		mode = "auto"
	}
	if mode == "staged" && !a.AfterBoot {
		saved.Staged, saved.StagedHash = &eff, hash
		if err := a.Store.Save(saved); err != nil {
			return Result{StateRejected, hash, err.Error()}
		}
		return Result{StateStaged, hash, ""}
	}
	return a.applyWithRevert(ctx, eff, hash, mode, saved)
}

func (a *Agent) applyWithRevert(ctx context.Context, eff Spec, hash, mode string, saved Saved) Result {
	var prev Spec
	if saved.Applied != nil {
		prev = *saved.Applied
	}
	oldR, newR := Render(prev), Render(eff)
	if a.rendered != nil {
		oldR = a.rendered
	}
	_, im := Diff(oldR, newR)
	timeout := defaultAuto
	if mode == "try" {
		timeout = defaultTry
	}
	if eff.TryTimeoutSeconds > 0 {
		timeout = time.Duration(eff.TryTimeoutSeconds) * time.Second
	}
	fail := func(cause error) Result {
		// auto-revert to the last-known-good config, then make it take effect again
		_ = a.Host.Write(oldR)
		_, back := Diff(newR, oldR)
		_ = a.Host.Apply(back)
		a.rendered = oldR
		return Result{StateRolledBack, hash, cause.Error()}
	}
	if err := a.Host.Write(newR); err != nil {
		return fail(fmt.Errorf("write: %w", err))
	}
	a.rendered = newR
	if err := a.Host.Apply(im); err != nil {
		return fail(fmt.Errorf("apply: %w", err))
	}
	if im.RestartK3s || mode == "try" {
		if err := a.Host.WaitHealthy(ctx, timeout); err != nil {
			return fail(fmt.Errorf("not healthy within %s: %w", timeout, err))
		}
	}
	saved.Applied, saved.AppliedHash = &eff, hash
	if saved.StagedHash == hash {
		saved.Staged, saved.StagedHash = nil, ""
	}
	if err := a.Store.Save(saved); err != nil {
		return Result{StateRejected, hash, "save last-known-good: " + err.Error()}
	}
	return Result{StateApplied, hash, ""}
}

// Restore re-creates the generated files from the last-known-good config (the sysctl and timesyncd drop-ins live in /run).
func (a *Agent) Restore() error {
	s := a.Store.Load()
	var sp Spec
	if s.Applied != nil {
		sp = *s.Applied
	}
	r := Render(sp)
	a.rendered = r
	return a.Host.Write(r)
}
