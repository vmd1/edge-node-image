package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/inspect"
)

// ServiceUnits are the only units Service may touch (status for all, start/stop/restart for the same list).
var ServiceUnits = []string{"k3s-agent.service", "tailscaled.service", "systemd-oomd.service", "systemd-timesyncd.service", "edge-external-ip.service"}

func (s *Server) roots() inspect.Roots {
	r := inspect.Default()
	r.Proc = s.P.Proc
	return r
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", " ")
	return string(b), err
}

func (s *Server) Inspect(ctx context.Context, req *pb.InspectRequest) (*pb.InspectResponse, error) {
	r := s.roots()
	var v any
	var err error
	switch req.Kind {
	case "memory":
		v, err = r.Memory()
	case "psi":
		v, err = r.PSI()
	case "mounts":
		v, err = r.Mounts()
	case "sysctl":
		v = r.Sysctl()
	case "modules":
		v, err = r.Modules()
	case "processes":
		v, err = r.Processes(req.Arg, int(req.Limit))
	case "netstat":
		v, err = r.Netstat()
	case "cgroups":
		v, err = r.Cgroups(req.Arg)
	case "image":
		st, e := s.Status(ctx, &pb.StatusRequest{})
		v, err = st, e
	case "config":
		v, err = s.GetConfig(ctx, &pb.GetConfigRequest{})
	case "boot":
		out, e := s.R.Run(ctx, "bootctl", "status", "--no-pager")
		v, err = map[string]string{"bootctl": out}, e
	case "tailscale":
		out, e := s.R.Run(ctx, "tailscale", "status", "--json", "--peers=false")
		var raw json.RawMessage = json.RawMessage(out)
		if e != nil || !json.Valid(raw) {
			v, err = map[string]string{"output": out}, e
		} else {
			v = raw
		}
	case "kubelet":
		out, e := s.R.Run(ctx, "systemctl", "show", s.P.K3sUnit, "-p", "ActiveState,SubState,MainPID,NRestarts,MemoryCurrent,ActiveEnterTimestamp,Result")
		v, err = parseShow(out), e
	case "time":
		out, e := s.R.Run(ctx, "timedatectl", "show", "--property=NTPSynchronized,Timezone,TimeUSec")
		sync, _ := s.R.Run(ctx, "timedatectl", "show-timesync", "--all")
		v, err = map[string]any{"clock": parseShow(out), "timesync": parseShow(sync)}, e
	case "services":
		units := ServiceUnits
		if req.Arg != "" {
			if !contains(ServiceUnits, req.Arg) && !contains(StatusUnits, req.Arg) {
				return nil, status.Errorf(codes.InvalidArgument, "unit %q is not on the allowlist", req.Arg)
			}
			units = []string{req.Arg}
		}
		res := map[string]map[string]string{}
		for _, u := range append(append([]string{}, units...), StatusUnits...) {
			if req.Arg != "" && u != req.Arg {
				continue
			}
			out, _ := s.R.Run(ctx, "systemctl", "show", u, "-p", "ActiveState,SubState,NRestarts,MemoryCurrent,ActiveEnterTimestamp")
			res[u] = parseShow(out)
		}
		v = res
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unknown kind %q (image, boot, memory, psi, mounts, sysctl, modules, processes, netstat, cgroups, services, tailscale, kubelet, time, config)", req.Kind)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	js, err := marshal(v)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.InspectResponse{Json: js}, nil
}

func parseShow(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (s *Server) ListFiles(_ context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	l, err := s.roots().List(req.Path)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	js, _ := marshal(l)
	return &pb.ListFilesResponse{Json: js}, nil
}

func (s *Server) ReadFile(_ context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	b, err := s.roots().Read(req.Path, int64(req.MaxBytes))
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &pb.ReadFileResponse{Data: b}, nil
}

func (s *Server) Service(ctx context.Context, req *pb.ServiceRequest) (*pb.ServiceResponse, error) {
	if !contains(ServiceUnits, req.Unit) && !(req.Action == "status" && contains(StatusUnits, req.Unit)) {
		return nil, status.Errorf(codes.InvalidArgument, "unit %q is not on the allowlist: %s", req.Unit, strings.Join(ServiceUnits, ", "))
	}
	switch req.Action {
	case "status":
		out, _ := s.R.Run(ctx, "systemctl", "status", "--no-pager", "-n", "10", req.Unit)
		return &pb.ServiceResponse{Output: out}, nil
	case "start", "stop", "restart":
		s.mu.Lock()
		defer s.mu.Unlock()
		out, err := s.R.Run(ctx, "systemctl", req.Action, req.Unit)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "systemctl %s %s: %v: %s", req.Action, req.Unit, err, out)
		}
		return &pb.ServiceResponse{Output: out}, nil
	}
	return nil, status.Errorf(codes.InvalidArgument, "action %q: want status, start, stop or restart", req.Action)
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// healthDir holds the same check scripts the boot gate runs.
const healthDir = "/usr/lib/edge/health.d"

func (s *Server) Health(ctx context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	var cs []check
	ents, _ := os.ReadDir(s.healthDir())
	for _, e := range ents {
		p := filepath.Join(s.healthDir(), e.Name())
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := s.R.Run(cctx, p)
		cancel()
		cs = append(cs, check{Name: strings.TrimPrefix(e.Name(), e.Name()[:3]), OK: err == nil, Detail: lastLine(out)})
	}
	for _, c := range []struct{ name, addr string }{{"https-listener", "127.0.0.1:443"}, {"http-listener", "127.0.0.1:80"}} {
		conn, err := net.DialTimeout("tcp", c.addr, 3*time.Second)
		ok := err == nil
		if ok {
			conn.Close()
		}
		cs = append(cs, check{Name: c.name, OK: ok})
	}
	if psi, err := s.roots().PSI(); err == nil {
		some := psiAvg10(psi["memory"])
		cs = append(cs, check{Name: "memory-pressure", OK: some < 10, Detail: fmt.Sprintf("some avg10=%.2f", some)})
	}
	ok := true
	for _, c := range cs {
		ok = ok && c.OK
	}
	js, _ := marshal(cs)
	return &pb.HealthResponse{Ok: ok, Json: js}, nil
}

func (s *Server) healthDir() string {
	if s.HealthDir != "" {
		return s.HealthDir
	}
	return healthDir
}

func psiAvg10(line string) float64 {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "avg10="); ok {
			x, _ := strconv.ParseFloat(v, 64)
			return x
		}
	}
	return 0
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return l[len(l)-1]
}

func (s *Server) Events(req *pb.EventsRequest, stream pb.Noded_EventsServer) error {
	n := req.TailLines
	if n == 0 {
		n = 100
	}
	if n > 2000 {
		n = 2000
	}
	args := []string{"--no-pager", "-o", "short-iso", "-n", strconv.Itoa(int(n)), "_PID=1"}
	if req.Follow {
		args = append(args, "-f")
	}
	return s.R.Stream(stream.Context(), func(l string) error { return stream.Send(&pb.LogLine{Text: l}) }, "journalctl", args...)
}

const maxSupport = 16 << 20

// Support streams a tar.gz of diagnostics, built in memory and capped (journals are tail-bounded). nodectl encrypts it.
func (s *Server) Support(req *pb.SupportRequest, stream pb.Noded_SupportServer) error {
	ctx := stream.Context()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name, content string) {
		if buf.Len() > maxSupport {
			return
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), ModTime: time.Now()})
		_, _ = tw.Write([]byte(content))
	}
	run := func(name string, cmd string, args ...string) {
		out, err := s.R.Run(ctx, cmd, args...)
		if err != nil {
			out += "\n[exit: " + err.Error() + "]"
		}
		add(name, out)
	}
	run("journal.txt", "journalctl", "--no-pager", "-o", "short-iso", "-n", "2000")
	run("dmesg.txt", "journalctl", "--no-pager", "-k", "-n", "1000")
	run("k3s-agent.txt", "journalctl", "--no-pager", "-o", "short-iso", "-u", "k3s-agent.service", "-n", "1000")
	run("noded.txt", "journalctl", "--no-pager", "-o", "short-iso", "-u", "noded.service", "-n", "500")
	run("units.txt", "systemctl", "list-units", "--no-pager", "--plain", "--all")
	run("failed.txt", "systemctl", "--failed", "--no-pager", "--plain")
	run("bootctl.txt", "bootctl", "status", "--no-pager")
	run("tailscale.txt", "tailscale", "status")
	for _, k := range []string{"image", "memory", "psi", "mounts", "sysctl", "modules", "processes", "netstat", "cgroups", "config", "kubelet", "time"} {
		arg := ""
		if k == "cgroups" {
			arg = "memory"
		}
		if r, err := s.Inspect(ctx, &pb.InspectRequest{Kind: k, Arg: arg}); err == nil {
			add("resource-"+k+".json", r.Json)
		}
	}
	if h, err := s.Health(ctx, &pb.HealthRequest{}); err == nil {
		add("health.json", h.Json)
	}
	_ = tw.Close()
	_ = gz.Close()
	b := buf.Bytes()
	for len(b) > 0 {
		n := min(len(b), 256<<10)
		if err := stream.Send(&pb.Chunk{Data: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

var pcapBusy = make(chan struct{}, 1)

func (s *Server) Pcap(req *pb.PcapRequest, stream pb.Noded_PcapServer) error {
	secs, maxB := req.Seconds, req.MaxBytes
	if secs == 0 {
		secs = 30
	}
	if maxB == 0 {
		maxB = 20 << 20
	}
	if secs > 120 || maxB > 50<<20 {
		return status.Error(codes.InvalidArgument, "capture is limited to 120 s and 50 MB")
	}
	if req.Proto != "" && req.Proto != "tcp" && req.Proto != "udp" && req.Proto != "icmp" {
		return status.Error(codes.InvalidArgument, "proto: want tcp, udp or icmp")
	}
	var host net.IP
	if req.Host != "" {
		if host = net.ParseIP(req.Host); host == nil {
			return status.Error(codes.InvalidArgument, "host must be an IP address")
		}
	}
	if req.Port > 65535 {
		return status.Error(codes.InvalidArgument, "port out of range")
	}
	iface := req.Iface
	if iface == "" {
		iface = "any"
	}
	if iface != "any" {
		if _, err := net.InterfaceByName(iface); err != nil {
			return status.Errorf(codes.InvalidArgument, "no interface %q", iface)
		}
	}
	select {
	case pcapBusy <- struct{}{}:
		defer func() { <-pcapBusy }()
	default:
		return status.Error(codes.ResourceExhausted, "another capture is running")
	}
	f := pcapFilter{host: host, port: uint16(req.Port), proto: req.Proto}
	ctx, cancel := context.WithTimeout(stream.Context(), time.Duration(secs)*time.Second)
	defer cancel()
	return capture(ctx, iface, f, int64(maxB), func(b []byte) error { return stream.Send(&pb.Chunk{Data: b}) })
}
