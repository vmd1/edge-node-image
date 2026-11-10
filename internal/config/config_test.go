package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func ip(i int) *int   { return &i }
func bp(b bool) *bool { return &b }

type fakeHost struct {
	written    []Rendered
	applied    []Impact
	healthErrs []error // consumed one per WaitHealthy call
}

func (f *fakeHost) Write(r Rendered) error { f.written = append(f.written, r); return nil }
func (f *fakeHost) Apply(im Impact) error  { f.applied = append(f.applied, im); return nil }
func (f *fakeHost) WaitHealthy(context.Context, time.Duration) error {
	if len(f.healthErrs) == 0 {
		return nil
	}
	e := f.healthErrs[0]
	f.healthErrs = f.healthErrs[1:]
	return e
}

func newAgent(t *testing.T, h *fakeHost) *Agent {
	return &Agent{Host: h, Store: Store{Path: t.TempDir() + "/saved.json"}}
}

func TestMergeMostSpecificWins(t *testing.T) {
	objs := []Object{
		{Name: "region", Spec: Spec{NodeSelector: map[string]string{"region": "uk"}, Kubelet: &Kubelet{MaxPods: ip(60)}}},
		{Name: "all", Spec: Spec{Kubelet: &Kubelet{MaxPods: ip(40), KubeReserved: map[string]string{"memory": "100Mi"}}}},
		{Name: "node", Spec: Spec{NodeSelector: map[string]string{"region": "uk", "name": "a"}, Kubelet: &Kubelet{KubeReserved: map[string]string{"memory": "150Mi"}}}},
		{Name: "other", Spec: Spec{NodeSelector: map[string]string{"region": "us"}, Kubelet: &Kubelet{MaxPods: ip(99)}}},
	}
	got := Merge(objs, map[string]string{"region": "uk", "name": "a"})
	if *got.Kubelet.MaxPods != 60 || got.Kubelet.KubeReserved["memory"] != "150Mi" {
		t.Fatalf("merge: %+v", got.Kubelet)
	}
}

func TestOverlayOverridesCRD(t *testing.T) {
	base := Merge([]Object{{Name: "a", Spec: Spec{Sysctls: map[string]string{"vm.swappiness": "100"}}}}, nil)
	Overlay(&base, Spec{Sysctls: map[string]string{"vm.swappiness": "60"}})
	if base.Sysctls["vm.swappiness"] != "60" {
		t.Fatal(base.Sysctls)
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []Spec{
		{Mode: "yolo"},
		{Kubelet: &Kubelet{MaxPods: ip(5)}},
		{Kubelet: &Kubelet{EvictionHard: map[string]string{"bogus": "1Mi"}}},
		{Kubelet: &Kubelet{SystemReserved: map[string]string{"memory": "lots"}}},
		{Kubelet: &Kubelet{ImageGCHigh: ip(50), ImageGCLow: ip(60)}},
		{NodeLabels: map[string]string{"kubernetes.io/hostname": "x"}},
		{NodeLabels: map[string]string{"node-type": "x"}},
		{Sysctls: map[string]string{"kernel.core_pattern": "1"}},
		{Sysctls: map[string]string{"vm.swappiness": "-1"}},
		{TimeServers: []string{"bad host!"}},
		{K3sLogLevel: ip(12)},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d accepted: %+v", i, s)
		}
	}
	ok := Spec{Mode: "try", Kubelet: &Kubelet{MaxPods: ip(60), EvictionHard: map[string]string{"memory.available": "100Mi"}},
		NodeLabels: map[string]string{"team": "edge"}, Sysctls: map[string]string{"vm.swappiness": "80"}, TimeServers: []string{"time.cloudflare.com"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRender(t *testing.T) {
	s := Spec{Kubelet: &Kubelet{SystemReserved: map[string]string{"memory": "120Mi"}, EvictionHard: map[string]string{"memory.available": "80Mi"}, MaxPods: ip(50)},
		NodeLabels: map[string]string{"a": "b"}, Sysctls: map[string]string{"vm.swappiness": "80"}, ModuleLockdown: bp(false)}
	r := Render(s)
	k := r[K3sDropin]
	for _, want := range []string{`kubelet-arg+:`, `"system-reserved=memory=120Mi"`, `"eviction-hard=memory.available<80Mi"`, `"max-pods=50"`, `node-label+:`, `"a=b"`} {
		if !strings.Contains(k, want) {
			t.Errorf("k3s drop-in lacks %s:\n%s", want, k)
		}
	}
	if !strings.Contains(r[SysctlDropin], "vm.swappiness = 80") || r[LockdownFlag] == "" {
		t.Fatalf("%v", r)
	}
	if strings.Contains(k, "token") || strings.Contains(k, "server") || strings.Contains(k, "taint") {
		t.Fatal("bootstrap items must never be rendered")
	}
}

func TestReconcileAppliesAndIsIdempotent(t *testing.T) {
	h := &fakeHost{}
	a := newAgent(t, h)
	eff := Spec{Kubelet: &Kubelet{MaxPods: ip(60)}}
	if r := a.Reconcile(context.Background(), eff); r.State != StateApplied {
		t.Fatalf("%+v", r)
	}
	if len(h.applied) != 1 || !h.applied[0].RestartK3s {
		t.Fatalf("expected one k3s restart: %+v", h.applied)
	}
	if r := a.Reconcile(context.Background(), eff); r.State != StateApplied || len(h.applied) != 1 {
		t.Fatalf("second reconcile must be a no-op: %+v %d", r, len(h.applied))
	}
}

func TestReconcileNothingConfiguredIsNoop(t *testing.T) {
	h := &fakeHost{}
	if r := newAgent(t, h).Reconcile(context.Background(), Spec{}); r.State != StateApplied || len(h.written) != 0 {
		t.Fatalf("%+v %d", r, len(h.written))
	}
}

func TestReconcileRejectsInvalidAndKeepsRunning(t *testing.T) {
	h := &fakeHost{}
	a := newAgent(t, h)
	r := a.Reconcile(context.Background(), Spec{Kubelet: &Kubelet{MaxPods: ip(3)}})
	if r.State != StateRejected || r.Error == "" || len(h.written) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReconcileRollsBackWhenUnhealthy(t *testing.T) {
	h := &fakeHost{healthErrs: []error{errors.New("node NotReady")}}
	a := newAgent(t, h)
	good := Spec{Kubelet: &Kubelet{MaxPods: ip(60)}}
	// first apply is healthy (no health error queued for it would consume the NotReady; queue order matters)
	h.healthErrs = nil
	if r := a.Reconcile(context.Background(), good); r.State != StateApplied {
		t.Fatalf("%+v", r)
	}
	h.healthErrs = []error{errors.New("node NotReady")}
	bad := Spec{Kubelet: &Kubelet{MaxPods: ip(200)}, Mode: "try"}
	r := a.Reconcile(context.Background(), bad)
	if r.State != StateRolledBack || !strings.Contains(r.Error, "NotReady") {
		t.Fatalf("%+v", r)
	}
	// the files on the node are the last-known-good ones again and k3s was restarted to pick them up
	last := h.written[len(h.written)-1]
	if !strings.Contains(last[K3sDropin], `max-pods=60`) {
		t.Fatalf("not reverted: %s", last[K3sDropin])
	}
	if !h.applied[len(h.applied)-1].RestartK3s {
		t.Fatal("k3s must restart after the revert")
	}
	if got := a.Store.Load(); got.Applied == nil || *got.Applied.Kubelet.MaxPods != 60 {
		t.Fatalf("last-known-good lost: %+v", got)
	}
}

func TestStagedWaitsForBoot(t *testing.T) {
	h := &fakeHost{}
	a := newAgent(t, h)
	eff := Spec{Mode: "staged", Kubelet: &Kubelet{MaxPods: ip(70)}}
	if r := a.Reconcile(context.Background(), eff); r.State != StateStaged || len(h.written) != 0 {
		t.Fatalf("%+v", r)
	}
	a.AfterBoot = true
	if r := a.Reconcile(context.Background(), eff); r.State != StateApplied || len(h.written) == 0 {
		t.Fatalf("%+v", r)
	}
	if s := a.Store.Load(); s.Staged != nil {
		t.Fatal("staged config must be cleared once applied")
	}
}

func TestRestoreRewritesRunFiles(t *testing.T) {
	h := &fakeHost{}
	a := newAgent(t, h)
	a.Reconcile(context.Background(), Spec{Sysctls: map[string]string{"vm.swappiness": "70"}})
	h2 := &fakeHost{}
	a2 := &Agent{Host: h2, Store: a.Store}
	if err := a2.Restore(); err != nil || !strings.Contains(h2.written[0][SysctlDropin], "vm.swappiness = 70") {
		t.Fatalf("%v %v", err, h2.written)
	}
}

func TestOverrideExpires(t *testing.T) {
	now := time.Now()
	o := &Overrides{Path: t.TempDir() + "/o.json", Now: func() time.Time { return now }}
	if _, err := o.Set(Spec{Sysctls: map[string]string{"vm.swappiness": "40"}}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if o.Active() == nil {
		t.Fatal("override should be active")
	}
	now = now.Add(2 * time.Hour)
	if o.Active() != nil {
		t.Fatal("override should have expired")
	}
	if _, err := o.Set(Spec{}, 8*24*time.Hour); err == nil {
		t.Fatal("ttl over 7d must be refused")
	}
	if _, err := o.Set(Spec{Mode: "bad"}, 0); err == nil {
		t.Fatal("invalid override must be refused")
	}
}
