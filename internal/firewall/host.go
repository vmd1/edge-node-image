package firewall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/vmd1/edge-node-image/internal/config"
)

const LKGPath = "/var/lib/edge/config/firewall.nft"

type SysHost struct {
	Kube *config.Kube
	LKG  string
}

func (h *SysHost) lkg() string {
	if h.LKG != "" {
		return h.LKG
	}
	return LKGPath
}

func (h *SysHost) Load(ctx context.Context, script string) error {
	c := exec.CommandContext(ctx, "nft", "-f", "-")
	c.Stdin = bytes.NewBufferString(script)
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %.300s", err, out)
	}
	return nil
}

func (h *SysHost) Verify(ctx context.Context) error {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _, _, err := h.Kube.Node(vctx)
	return err
}

func (h *SysHost) ReadLKG() string { b, _ := os.ReadFile(h.lkg()); return string(b) }

func (h *SysHost) WriteLKG(s string) error {
	if err := os.MkdirAll(filepath.Dir(h.lkg()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.lkg(), []byte(s), 0o644)
}

// Rules lists FirewallRule objects.
func Rules(ctx context.Context, k *config.Kube) ([]Rule, error) {
	var l struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec Rule `json:"spec"`
		} `json:"items"`
	}
	if err := k.GetJSON(ctx, "/apis/network.vmd1.dev/v1alpha1/firewallrules", &l); err != nil {
		return nil, err
	}
	var out []Rule
	for _, i := range l.Items {
		r := i.Spec
		r.Name = i.Metadata.Name
		out = append(out, r)
	}
	return out, nil
}

// Boot applies the last-known-good ruleset, or the baseline when there is none, before the API is reachable.
func Boot(ctx context.Context, h Host, node string) {
	script := h.ReadLKG()
	if script == "" {
		script, _ = Build(nil, node, nil)
	}
	if err := h.Load(ctx, script); err != nil {
		log.Printf("firewall: boot ruleset: %v", err)
	}
}

// Loop keeps the host firewall in step with the FirewallRule objects. If the API is unreachable the running chain stays.
func Loop(ctx context.Context, k *config.Kube, h Host, every time.Duration) {
	for {
		step(ctx, k, h)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func step(ctx context.Context, k *config.Kube, h Host) {
	labels, ann, _, err := k.Node(ctx)
	if err != nil {
		return
	}
	rules, err := Rules(ctx, k)
	if err != nil {
		log.Printf("firewall: list FirewallRule: %v (keeping the running chain)", err)
		return
	}
	script, errs := Build(rules, k.NodeName, labels)
	res := Apply(ctx, h, script)
	msg := res.Error
	for _, e := range errs {
		msg += "; skipped: " + e.Error()
	}
	if res.State != "unchanged" || ann["node.vmd1.dev/firewall-state"] == "" {
		state := res.State
		if state == "unchanged" {
			state = "applied"
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{"node.vmd1.dev/firewall-state": state, "node.vmd1.dev/firewall-error": trunc(msg)}}})
		_ = k.Patch(ctx, patch)
		log.Printf("firewall: %s %s", res.State, msg)
	}
}

func trunc(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
