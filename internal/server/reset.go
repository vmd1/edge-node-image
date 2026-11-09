package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

// resetTargets are what each wipe mode deletes, relative to the root. /var itself (the mount), lost+found and the root and
// boot partitions are never touched.
func resetTargets(mode string) []string {
	switch mode {
	case "all-var":
		return []string{"/var/lib", "/var/log", "/var/cache", "/var/tmp", "/var/spool"}
	default:
		return []string{"/var/lib/rancher", "/var/lib/kubelet", "/var/lib/cni", "/var/lib/containerd", "/run/k3s"}
	}
}

// keep lists paths (relative to /var/lib/rancher) that survive a k3s wipe so the node rejoins under the same name.
func keptInK3sWipe(path string) bool {
	return strings.Contains(path, "node-password")
}

func (s *Server) rootDir() string { return s.Root }

func validWipe(w string) (string, error) {
	switch w {
	case "", "k3s":
		return "k3s", nil
	case "all-var":
		return "all-var", nil
	}
	return "", status.Errorf(codes.InvalidArgument, "wipe %q: want k3s or all-var", w)
}

// imdsMetadata fetches the instance metadata the way edge-firstboot does.
func (s *Server) imdsMetadata(ctx context.Context) (map[string]string, error) {
	url := s.IMDSURL
	if url == "" {
		url = "http://169.254.169.254/opc/v2/instance/"
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer Oracle")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("instance metadata: %s", resp.Status)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d struct {
		DisplayName string            `json:"displayName"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.Metadata["_displayName"] = d.DisplayName
	return d.Metadata, nil
}

func (s *Server) ResetPreflight(ctx context.Context, req *pb.ResetRequest) (*pb.ResetPreflightResponse, error) {
	mode, err := validWipe(req.Wipe)
	if err != nil {
		return nil, err
	}
	if mode == "k3s" {
		return &pb.ResetPreflightResponse{Ok: true, Detail: "k3s wipe keeps the Tailscale identity and node password; nothing to pre-check"}, nil
	}
	md, err := s.imdsMetadata(ctx)
	if err != nil {
		return &pb.ResetPreflightResponse{Ok: false, Detail: "first boot could not read instance metadata, so an all-var wipe would strand the node: " + err.Error()}, nil
	}
	var missing []string
	for _, k := range []string{"tailscale_authkey", "k3s_token"} {
		if md[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return &pb.ResetPreflightResponse{Ok: false, Detail: "instance metadata lacks " + strings.Join(missing, ", ") + " (first boot would not be able to rejoin)"}, nil
	}
	return &pb.ResetPreflightResponse{Ok: true, Detail: "instance metadata has the Tailscale auth key and the k3s token"}, nil
}

// wipeUnder removes the children of dir, skipping keep(path) matches, and returns what it removed.
func wipeUnder(root, dir string, keep func(string) bool) ([]string, error) {
	var removed []string
	base := filepath.Join(root, dir)
	var walk func(p string) error
	walk = func(p string) error {
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if keep != nil && keep(p) {
			return nil
		}
		if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			ents, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			for _, e := range ents {
				if err := walk(filepath.Join(p, e.Name())); err != nil {
					return err
				}
			}
			if keep != nil {
				// a directory that still contains kept files stays
				if rest, _ := os.ReadDir(p); len(rest) > 0 {
					return nil
				}
			}
			if p == base {
				return nil // the target directory itself stays; its contents go
			}
		}
		if p == base {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		removed = append(removed, strings.TrimPrefix(p, root))
		return nil
	}
	return removed, walk(base)
}

func (s *Server) Reset(ctx context.Context, req *pb.ResetRequest) (*pb.ResetResponse, error) {
	mode, err := validWipe(req.Wipe)
	if err != nil {
		return nil, err
	}
	if mode == "all-var" {
		pf, _ := s.ResetPreflight(ctx, req)
		if !pf.Ok {
			return nil, status.Error(codes.FailedPrecondition, pf.Detail)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if out, err := s.R.Run(ctx, "systemctl", "stop", s.P.K3sUnit); err != nil {
		return nil, status.Errorf(codes.Internal, "stop %s: %v: %s", s.P.K3sUnit, err, out)
	}
	// kubelet and CNI leave mounts under their directories; detach them (deepest first) before deleting
	if mounts, err := s.roots().Mounts(); err == nil {
		var pts []string
		for _, m := range mounts {
			if strings.HasPrefix(m.Point, "/var/lib/kubelet/") || strings.HasPrefix(m.Point, "/var/lib/rancher/") || strings.HasPrefix(m.Point, "/run/k3s/") {
				pts = append(pts, m.Point)
			}
		}
		sort.Slice(pts, func(i, j int) bool { return len(pts[i]) > len(pts[j]) })
		for _, p := range pts {
			_, _ = s.R.Run(ctx, "umount", "-l", p)
		}
	}
	var removed []string
	for _, t := range resetTargets(mode) {
		var keep func(string) bool
		if mode == "k3s" && strings.HasPrefix(t, "/var/lib/rancher") {
			keep = keptInK3sWipe
		}
		r, err := wipeUnder(s.rootDir(), t, keep)
		removed = append(removed, r...)
		if err != nil {
			return &pb.ResetResponse{Removed: removed}, status.Errorf(codes.Internal, "wipe %s: %v", t, err)
		}
	}
	reboot := req.Reboot || mode == "all-var"
	if reboot {
		go func() {
			time.Sleep(2 * time.Second) // let this response reach the caller
			_, _ = s.R.Run(context.Background(), "systemctl", "--no-block", "reboot")
		}()
	}
	return &pb.ResetResponse{Removed: removed, Rebooting: reboot}, nil
}
