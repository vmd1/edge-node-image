package firewall

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeHost struct {
	loaded    []string
	lkg       string
	loadErr   error
	verifyErr error
}

func (f *fakeHost) Load(_ context.Context, s string) error {
	if f.loadErr != nil {
		return f.loadErr
	}
	f.loaded = append(f.loaded, s)
	return nil
}
func (f *fakeHost) Verify(context.Context) error { return f.verifyErr }
func (f *fakeHost) ReadLKG() string              { return f.lkg }
func (f *fakeHost) WriteLKG(s string) error      { f.lkg = s; return nil }

func TestBaselineAndTrusted(t *testing.T) {
	s, errs := Build(nil, "n1", nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, want := range []string{"policy drop", `iifname { "lo", "tailscale0", "cni0"`, "ip saddr 10.42.0.0/16 accept", "tcp dport 443 accept", "udp dport 53 accept",
		"tcp dport 53 accept", "udp dport 41641 accept", "ct state established,related accept", "delete table inet edge"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	for _, never := range []string{"10250", "9100", "22 accept"} {
		if strings.Contains(s, never) {
			t.Errorf("baseline must not open %s", never)
		}
	}
}

func TestRulesSelectByTypeNodeAndLabels(t *testing.T) {
	rules := []Rule{
		{Name: "oci-only", Type: "OCI", Ports: []Port{{8080, "TCP"}}},
		{Name: "host-all", Type: "Host", Ports: []Port{{8443, "TCP"}}},
		{Name: "host-ingress", Type: "Host", NodeSelector: map[string]string{"role": "ingress"}, Ports: []Port{{9000, "UDP"}}, Sources: []string{"192.168.0.0/24", "2001:db8::/32"}},
		{Name: "other-node", Type: "Host", NodeName: "n2", Ports: []Port{{7000, "TCP"}}},
	}
	s, _ := Build(rules, "n1", map[string]string{"role": "ingress"})
	if strings.Contains(s, "8080") || strings.Contains(s, "7000") {
		t.Fatalf("OCI rules and other nodes' rules must not apply:\n%s", s)
	}
	for _, want := range []string{"tcp dport 8443 accept", "ip saddr 192.168.0.0/24 udp dport 9000 accept", "ip6 saddr 2001:db8::/32 udp dport 9000 accept"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if s2, _ := Build(rules, "n1", map[string]string{"role": "other"}); strings.Contains(s2, "9000") {
		t.Fatal("selector must exclude a node without the label")
	}
}

func TestInvalidRulesAreSkippedNotInjected(t *testing.T) {
	rules := []Rule{
		{Name: "inject-src", Type: "Host", Ports: []Port{{80, "TCP"}}, Sources: []string{"1.2.3.4; flush ruleset"}},
		{Name: "inject-proto", Type: "Host", Ports: []Port{{80, "tcp; drop"}}},
		{Name: "bad-port", Type: "Host", Ports: []Port{{70000, "TCP"}}},
		{Name: "empty", Type: "Host"},
		{Name: "good", Type: "Host", Ports: []Port{{2222, "TCP"}}},
	}
	s, errs := Build(rules, "n", nil)
	if len(errs) != 4 {
		t.Fatalf("errs: %v", errs)
	}
	if strings.Contains(s, "flush") || strings.Contains(s, "drop;") && strings.Count(s, "drop") > 2 || strings.Contains(s, "70000") {
		t.Fatalf("invalid input reached the ruleset:\n%s", s)
	}
	if !strings.Contains(s, "tcp dport 2222 accept") {
		t.Fatal("valid rules must still apply when others are invalid")
	}
}

func TestAnywhereCIDRIsPlainAccept(t *testing.T) {
	s, _ := Build([]Rule{{Name: "x", Type: "Host", Ports: []Port{{5000, "TCP"}}, Sources: []string{"0.0.0.0/0"}}}, "n", nil)
	if !strings.Contains(s, "  tcp dport 5000 accept") {
		t.Fatal(s)
	}
}

func TestApplyPaths(t *testing.T) {
	ctx := context.Background()
	h := &fakeHost{lkg: "old"}
	if r := Apply(ctx, h, "old"); r.State != "unchanged" || len(h.loaded) != 0 {
		t.Fatalf("%+v", r)
	}
	if r := Apply(ctx, h, "new"); r.State != "applied" || h.lkg != "new" {
		t.Fatalf("%+v", r)
	}
	h = &fakeHost{lkg: "old", loadErr: errors.New("syntax error")}
	if r := Apply(ctx, h, "bad"); r.State != "rejected" || h.lkg != "old" {
		t.Fatalf("a rejected script must leave the last-known-good alone: %+v", r)
	}
}

func TestApplyRevertsWhenVerificationFails(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the verify retries")
	}
	verifyWait = 0
	h := &fakeHost{lkg: "old", verifyErr: errors.New("api unreachable")}
	r := Apply(context.Background(), h, "new")
	if r.State != "rolled-back" || h.lkg != "old" {
		t.Fatalf("%+v", r)
	}
	if len(h.loaded) != 2 || h.loaded[1] != "old" {
		t.Fatalf("the previous ruleset must be loaded again: %v", h.loaded)
	}
}

func TestExtraBaselineFromEnv(t *testing.T) {
	t.Setenv("EDGE_FIREWALL_EXTRA_PORTS", "7777/tcp, 8080/udp,bad,99999/tcp,22/sctp")
	s, _ := Build(nil, "n", nil)
	if !strings.Contains(s, "tcp dport 7777 accept") || !strings.Contains(s, "udp dport 8080 accept") || strings.Contains(s, "99999") || strings.Contains(s, "sctp") {
		t.Fatal(s)
	}
}
