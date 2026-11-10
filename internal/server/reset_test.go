package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

func resetFixture(t *testing.T) (*Server, *fakeRunner, string) {
	s, r, _ := fixture(t)
	root := t.TempDir()
	s.Root = root
	for _, f := range []string{"var/lib/rancher/k3s/agent/containerd/x", "var/lib/rancher/k3s/agent/node-password", "var/lib/rancher/k3s/data/y",
		"var/lib/kubelet/pods/p", "var/lib/tailscale/tailscaled.state", "var/lib/edge/bootstrap.env", "var/log/journal/j", "var/lib/cni/c"} {
		p := filepath.Join(root, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	return s, r, root
}

func exists(root, rel string) bool { _, err := os.Stat(filepath.Join(root, rel)); return err == nil }

func TestResetK3sKeepsTailscaleAndNodePassword(t *testing.T) {
	s, r, root := resetFixture(t)
	resp, err := s.Reset(context.Background(), &pb.ResetRequest{Wipe: "k3s"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Rebooting {
		t.Fatal("reboot=false must not reboot")
	}
	for _, gone := range []string{"var/lib/rancher/k3s/data/y", "var/lib/kubelet/pods/p", "var/lib/cni/c", "var/lib/rancher/k3s/agent/containerd/x"} {
		if exists(root, gone) {
			t.Errorf("%s should be wiped", gone)
		}
	}
	for _, kept := range []string{"var/lib/rancher/k3s/agent/node-password", "var/lib/tailscale/tailscaled.state", "var/lib/edge/bootstrap.env", "var/log/journal/j"} {
		if !exists(root, kept) {
			t.Errorf("%s must survive a k3s wipe", kept)
		}
	}
	if r.calls[0].name != "systemctl" || r.calls[0].args[0] != "stop" {
		t.Fatalf("k3s must be stopped first: %v", r.calls)
	}
}

func TestResetAllVarNeedsMetadata(t *testing.T) {
	s, _, root := resetFixture(t)
	s.IMDSURL = "http://127.0.0.1:1/" // unreachable
	if _, err := s.Reset(context.Background(), &pb.ResetRequest{Wipe: "all-var"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("all-var without reachable metadata must be refused: %v", err)
	}
	if !exists(root, "var/lib/tailscale/tailscaled.state") {
		t.Fatal("nothing may be wiped when the pre-flight fails")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"displayName":"n","metadata":{"k3s_token":"t"}}`))
	}))
	defer bad.Close()
	s.IMDSURL = bad.URL
	pf, _ := s.ResetPreflight(context.Background(), &pb.ResetRequest{Wipe: "all-var"})
	if pf.Ok || !strings.Contains(pf.Detail, "tailscale_authkey") {
		t.Fatalf("%+v", pf)
	}

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer Oracle" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"displayName":"n","metadata":{"k3s_token":"t","tailscale_authkey":"k"}}`))
	}))
	defer good.Close()
	s.IMDSURL = good.URL
	resp, err := s.Reset(context.Background(), &pb.ResetRequest{Wipe: "all-var"})
	if err != nil || !resp.Rebooting {
		t.Fatalf("%v %+v", err, resp)
	}
	for _, gone := range []string{"var/lib/tailscale/tailscaled.state", "var/lib/edge/bootstrap.env", "var/lib/rancher/k3s/agent/node-password", "var/log/journal/j"} {
		if exists(root, gone) {
			t.Errorf("%s should be wiped by all-var", gone)
		}
	}
	if !exists(root, "var") {
		t.Fatal("the /var mount point itself must stay")
	}
}

func TestResetRejectsUnknownMode(t *testing.T) {
	s, _, _ := resetFixture(t)
	if _, err := s.Reset(context.Background(), &pb.ResetRequest{Wipe: "everything"}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
}

func TestAllVarNeedsAdmin(t *testing.T) {
	if got := RequiredRole("/noded.v1.Noded/Reset", &pb.ResetRequest{Wipe: "all-var"}); got.String() != "admin" {
		t.Fatal(got)
	}
	if got := RequiredRole("/noded.v1.Noded/Reset", &pb.ResetRequest{Wipe: "k3s"}); got.String() != "operator" {
		t.Fatal(got)
	}
}
