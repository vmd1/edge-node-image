// Package server implements the noded API. Every operation is typed; there is deliberately no RPC that runs an
// arbitrary command or opens a shell.
package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/auth"
)

// Paths are overridable so tests can point at a fixture tree.
type Paths struct {
	OSRelease string // /etc/os-release
	Proc      string // /proc
	Run       string // /run/edge
	Var       string // /var/lib/edge
	EFIVars   string // /sys/firmware/efi/efivars
	K3sUnit   string // unit name of the k3s agent
}

func DefaultPaths() Paths {
	return Paths{OSRelease: "/etc/os-release", Proc: "/proc", Run: "/run/edge", Var: "/var/lib/edge",
		EFIVars: "/sys/firmware/efi/efivars", K3sUnit: "k3s-agent.service"}
}

// Units reported by Status and readable through Logs.
var StatusUnits = []string{"tailscaled.service", "k3s-agent.service", "noded.service", "systemd-networkd.service", "edge-health.service"}
var LogUnits = []string{"tailscaled.service", "k3s-agent.service", "noded.service", "systemd-networkd.service", "edge-health.service", "edge-firstboot.service", "systemd-sysupdate.service", "kernel"}

// allowHTTP is set true only by the debug build (see http_debug.go).
var allowHTTP = false

var (
	versionRe  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)
	loaderGUID = "4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"
)

type Server struct {
	pb.UnimplementedNodedServer
	P     Paths
	R     Runner
	mu    sync.Mutex // one mutating operation at a time
	Units []string   // units reported by Status
}

func New(p Paths, r Runner) *Server { return &Server{P: p, R: r, Units: StatusUnits} }

// Register installs the server with the authorizer applied to every call.
func (s *Server) Register(g *grpc.Server) { pb.RegisterNodedServer(g, s) }

func Interceptors(a auth.Authorizer) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := a.Authorize(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := a.Authorize(ss.Context()); err != nil {
				return err
			}
			return h(srv, ss)
		}),
	}
}

func (s *Server) osRelease(key string) string {
	f, err := os.Open(s.P.OSRelease)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && k == key {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// efiString reads a UTF-16 EFI variable (4 attribute bytes first).
func efiString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 6 {
		return ""
	}
	b = b[4:]
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// psiAvg10 returns the "some" avg10 value of /proc/pressure/<name>.
func (s *Server) psiAvg10(name string) float64 {
	for _, line := range strings.Split(readTrim(filepath.Join(s.P.Proc, "pressure", name)), "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, _ := strconv.ParseFloat(v, 64)
				return x
			}
		}
	}
	return 0
}

func (s *Server) memAvailable() uint64 {
	for _, line := range strings.Split(readTrim(filepath.Join(s.P.Proc, "meminfo")), "\n") {
		if v, ok := strings.CutPrefix(line, "MemAvailable:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				n, _ := strconv.ParseUint(f[0], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
}

func (s *Server) buildStatus(ctx context.Context) *pb.StatusResponse {
	entry := efiString(filepath.Join(s.P.EFIVars, "LoaderEntrySelected-"+loaderGUID))
	assess := "unknown"
	switch {
	case entry == "":
	case strings.Contains(entry, "+"):
		assess = "pending"
	default:
		assess = "good"
	}
	up := strings.Fields(readTrim(filepath.Join(s.P.Proc, "uptime")))
	var uptime uint64
	if len(up) > 0 {
		f, _ := strconv.ParseFloat(up[0], 64)
		uptime = uint64(f)
	}
	units := map[string]string{}
	for _, u := range s.Units {
		out, _ := s.R.Run(ctx, "systemctl", "show", "-p", "ActiveState", "--value", u)
		units[u] = strings.TrimSpace(out)
	}
	return &pb.StatusResponse{
		ImageVersion:         s.osRelease("IMAGE_VERSION"),
		BootedEntry:          entry,
		BootAssessment:       assess,
		AvailableVersion:     readTrim(filepath.Join(s.P.Run, "available-version")),
		MemPressureSomeAvg10: s.psiAvg10("memory"),
		IoPressureSomeAvg10:  s.psiAvg10("io"),
		MemAvailableBytes:    s.memAvailable(),
		Units:                units,
		ExternalIp:           readTrim(filepath.Join(s.P.Run, "external-ip")),
		UptimeSeconds:        uptime,
		BootId:               readTrim(filepath.Join(s.P.Proc, "sys", "kernel", "random", "boot_id")),
	}
}

func (s *Server) Status(ctx context.Context, _ *pb.StatusRequest) (*pb.StatusResponse, error) {
	return s.buildStatus(ctx), nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) Logs(req *pb.LogsRequest, stream pb.Noded_LogsServer) error {
	if !contains(LogUnits, req.Unit) {
		return status.Errorf(codes.InvalidArgument, "unit %q is not readable; allowed: %s", req.Unit, strings.Join(LogUnits, ", "))
	}
	n := req.TailLines
	if n == 0 {
		n = 100
	}
	if n > 2000 {
		n = 2000
	}
	args := []string{"--no-pager", "-o", "short-iso", "-n", strconv.Itoa(int(n))}
	if req.Unit == "kernel" {
		args = append(args, "-k")
	} else {
		args = append(args, "-u", req.Unit)
	}
	if req.Follow {
		args = append(args, "-f")
	}
	return s.R.Stream(stream.Context(), func(l string) error { return stream.Send(&pb.LogLine{Text: l}) }, "journalctl", args...)
}

func (s *Server) CheckUpdate(ctx context.Context, _ *pb.CheckUpdateRequest) (*pb.CheckUpdateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.R.Run(ctx, "/usr/libexec/edge-sysupdate", "--no-pager", "check-new")
	newest := ""
	if err == nil {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		newest = strings.TrimSpace(lines[len(lines)-1])
	}
	_ = os.MkdirAll(s.P.Run, 0o755)
	_ = os.WriteFile(filepath.Join(s.P.Run, "available-version"), []byte(newest+"\n"), 0o644)
	return &pb.CheckUpdateResponse{CurrentVersion: s.osRelease("IMAGE_VERSION"), NewestVersion: newest, Output: out}, nil
}

func (s *Server) StageUpdate(ctx context.Context, req *pb.StageUpdateRequest) (*pb.StageUpdateResponse, error) {
	if req.TargetVersion != "" && !versionRe.MatchString(req.TargetVersion) {
		return nil, status.Error(codes.InvalidArgument, "invalid version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	args := []string{"--no-pager", "update"}
	if req.TargetVersion != "" {
		args = append(args, req.TargetVersion)
	}
	out, err := s.R.Run(ctx, "/usr/libexec/edge-sysupdate", args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sysupdate failed: %v\n%s", err, out)
	}
	staged := req.TargetVersion
	if staged == "" {
		if m := regexp.MustCompile(`(?m)update '([^']+)'`).FindStringSubmatch(out); m != nil {
			staged = m[1]
		}
	}
	return &pb.StageUpdateResponse{StagedVersion: staged, Output: out}, nil
}

func (s *Server) GetUpdateConfig(context.Context, *pb.GetUpdateConfigRequest) (*pb.UpdateConfig, error) {
	return &pb.UpdateConfig{UpdateUrl: readTrim(filepath.Join(s.P.Var, "update-url"))}, nil
}

func validURL(raw string, https bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && !https)
}

func (s *Server) SetUpdateConfig(_ context.Context, c *pb.UpdateConfig) (*pb.SetUpdateConfigResponse, error) {
	path := filepath.Join(s.P.Var, "update-url")
	if c.UpdateUrl == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &pb.SetUpdateConfigResponse{}, nil
	}
	if !validURL(c.UpdateUrl, !allowHTTP) {
		return nil, status.Error(codes.InvalidArgument, "update_url must be an https URL without credentials or query")
	}
	u := c.UpdateUrl
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	if err := os.MkdirAll(s.P.Var, 0o755); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SetUpdateConfigResponse{}, os.WriteFile(path, []byte(u+"\n"), 0o644)
}

func (s *Server) Reboot(ctx context.Context, _ *pb.RebootRequest) (*pb.RebootResponse, error) {
	// Delay so this RPC can return before the machine goes down.
	if out, err := s.R.Run(ctx, "systemd-run", "--on-active=3", "--unit=edge-reboot", "systemctl", "reboot"); err != nil {
		return nil, status.Errorf(codes.Internal, "%v: %s", err, out)
	}
	return &pb.RebootResponse{}, nil
}

func (s *Server) SetBootSlot(ctx context.Context, req *pb.SetBootSlotRequest) (*pb.SetBootSlotResponse, error) {
	if !versionRe.MatchString(req.Version) {
		return nil, status.Error(codes.InvalidArgument, "invalid version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cmd := "set-default"
	if req.OneShot {
		cmd = "set-oneshot"
	}
	entry := "edge-node_" + req.Version + ".efi"
	if out, err := s.R.Run(ctx, "bootctl", cmd, entry); err != nil {
		return nil, status.Errorf(codes.Internal, "bootctl %s %s: %v\n%s", cmd, entry, err, out)
	}
	return &pb.SetBootSlotResponse{}, nil
}

type registryFile struct {
	DockerIOMirror   string `json:"docker_io_mirror"`
	DockerIOFallback string `json:"docker_io_fallback"`
}

func (s *Server) loadRegistry() registryFile {
	r := registryFile{DockerIOMirror: "https://mirror.gcr.io", DockerIOFallback: "https://registry-1.docker.io"}
	if b, err := os.ReadFile(filepath.Join(s.P.Var, "registry.json")); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func (s *Server) GetRegistryConfig(context.Context, *pb.GetRegistryConfigRequest) (*pb.RegistryConfig, error) {
	r := s.loadRegistry()
	return &pb.RegistryConfig{DockerIoMirror: r.DockerIOMirror, DockerIoFallback: r.DockerIOFallback}, nil
}

// RenderRegistries returns the k3s registries.yaml for a config.
func RenderRegistries(mirror, fallback string) string {
	if mirror == "" {
		return "# no mirror: docker.io is pulled directly\n"
	}
	var b strings.Builder
	b.WriteString("mirrors:\n  docker.io:\n    endpoint:\n")
	fmt.Fprintf(&b, "      - %q\n", mirror)
	if fallback != "" {
		fmt.Fprintf(&b, "      - %q\n", fallback)
	}
	return b.String()
}

func (s *Server) SetRegistryConfig(ctx context.Context, c *pb.RegistryConfig) (*pb.SetRegistryConfigResponse, error) {
	for _, u := range []string{c.DockerIoMirror, c.DockerIoFallback} {
		if u != "" && !validURL(u, true) {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not an https URL", u)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.P.Var, 0o755); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	b, _ := json.Marshal(registryFile{c.DockerIoMirror, c.DockerIoFallback})
	if err := os.WriteFile(filepath.Join(s.P.Var, "registry.json"), b, 0o644); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.WriteFile(filepath.Join(s.P.Var, "registries.yaml"), []byte(RenderRegistries(c.DockerIoMirror, c.DockerIoFallback)), 0o644); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	restarted := false
	if _, err := s.R.Run(ctx, "systemctl", "cat", s.P.K3sUnit); err == nil {
		if out, err := s.R.Run(ctx, "systemctl", "restart", s.P.K3sUnit); err != nil {
			return nil, status.Errorf(codes.Internal, "restart %s: %v\n%s", s.P.K3sUnit, err, out)
		}
		restarted = true
	}
	return &pb.SetRegistryConfigResponse{K3SAgentRestarted: restarted}, nil
}
