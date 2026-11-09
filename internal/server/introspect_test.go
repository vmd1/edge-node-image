package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

type chunkStream struct {
	grpc.ServerStream
	ctx  context.Context
	data bytes.Buffer
}

func (c *chunkStream) Context() context.Context { return c.ctx }
func (c *chunkStream) Send(m *pb.Chunk) error   { c.data.Write(m.Data); return nil }

func TestInspectKindsAndRefusals(t *testing.T) {
	s, r, p := fixture(t)
	r.out = map[string]string{"systemctl show": "ActiveState=active\nSubState=running\nNRestarts=0\n"}
	os.WriteFile(filepath.Join(p.Proc, "meminfo"), []byte("MemTotal: 975000 kB\nMemAvailable: 400000 kB\n"), 0o644)
	ctx := context.Background()
	for _, k := range []string{"memory", "psi", "sysctl", "mounts", "services", "kubelet", "image", "config"} {
		if _, err := s.Inspect(ctx, &pb.InspectRequest{Kind: k}); err != nil && k != "psi" && k != "mounts" {
			t.Errorf("%s: %v", k, err)
		}
	}
	if m, _ := s.Inspect(ctx, &pb.InspectRequest{Kind: "memory"}); !strings.Contains(m.Json, "MemTotalMiB") {
		t.Fatal(m.Json)
	}
	if _, err := s.Inspect(ctx, &pb.InspectRequest{Kind: "../etc/passwd"}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	if _, err := s.Inspect(ctx, &pb.InspectRequest{Kind: "services", Arg: "sshd.service"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("services for a non-allowlisted unit must be refused: %v", err)
	}
}

func TestServiceAllowlist(t *testing.T) {
	s, r, _ := fixture(t)
	ctx := context.Background()
	for _, bad := range []*pb.ServiceRequest{{Action: "restart", Unit: "sshd.service"}, {Action: "restart", Unit: "noded.service"},
		{Action: "mask", Unit: "k3s-agent.service"}, {Action: "stop", Unit: "edge-health.service"}} {
		if _, err := s.Service(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	if len(r.calls) != 0 {
		t.Fatalf("refused requests must not run anything: %v", r.calls)
	}
	if _, err := s.Service(ctx, &pb.ServiceRequest{Action: "restart", Unit: "k3s-agent.service"}); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0]; got.name != "systemctl" || strings.Join(got.args, " ") != "restart k3s-agent.service" {
		t.Fatalf("%+v", got)
	}
	// status of a status-only unit is fine, restart is not
	if _, err := s.Service(ctx, &pb.ServiceRequest{Action: "status", Unit: "noded.service"}); err != nil {
		t.Fatal(err)
	}
}

func TestFilesThroughServer(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.ReadFile(context.Background(), &pb.ReadFileRequest{Path: "/etc/shadow"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%v", err)
	}
	if _, err := s.ListFiles(context.Background(), &pb.ListFilesRequest{Path: "/root"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("%v", err)
	}
}

func TestSupportBundleIsATarGz(t *testing.T) {
	s, _, _ := fixture(t)
	cs := &chunkStream{ctx: context.Background()}
	if err := s.Support(&pb.SupportRequest{}, cs); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&cs.data)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	for _, want := range []string{"journal.txt", "dmesg.txt", "k3s-agent.txt", "units.txt", "resource-memory.json", "health.json"} {
		if !names[want] {
			t.Errorf("bundle lacks %s: %v", want, names)
		}
	}
}

func TestPcapValidation(t *testing.T) {
	s, _, _ := fixture(t)
	cs := &chunkStream{ctx: context.Background()}
	for _, bad := range []*pb.PcapRequest{{Seconds: 500}, {MaxBytes: 1 << 30}, {Proto: "sctp"}, {Host: "not-an-ip"}, {Port: 70000}, {Iface: "nope0"}} {
		if err := s.Pcap(bad, cs); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
}

func TestPcapFilter(t *testing.T) {
	ip4 := func(proto byte, src, dst string, sport, dport uint16) []byte {
		b := make([]byte, 40)
		b[0], b[9] = 0x45, proto
		copy(b[12:], net.ParseIP(src).To4())
		copy(b[16:], net.ParseIP(dst).To4())
		b[20], b[21], b[22], b[23] = byte(sport>>8), byte(sport), byte(dport>>8), byte(dport)
		return b
	}
	f := pcapFilter{host: net.ParseIP("10.0.0.5"), port: 53, proto: "udp"}
	if !f.matchIP(ip4(17, "10.0.0.5", "1.1.1.1", 4000, 53)) {
		t.Fatal("should match")
	}
	for _, p := range [][]byte{ip4(6, "10.0.0.5", "1.1.1.1", 4000, 53), ip4(17, "10.0.0.6", "1.1.1.1", 4000, 53), ip4(17, "10.0.0.5", "1.1.1.1", 4000, 80)} {
		if f.matchIP(p) {
			t.Fatal("should not match")
		}
	}
	if (pcapFilter{}).matchIP([]byte{1, 2}) {
		t.Fatal("short packet")
	}
}
