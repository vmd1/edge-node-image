// Package auth decides which peers may call noded. Release builds only ever use the Tailscale authorizer, which asks
// tailscaled who the peer is (WhoIs) and checks the answer against an allowlist. The default allowlist is empty, so a
// node with no configuration refuses everyone.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Authorizer reports whether the caller behind ctx may use the API.
type Authorizer interface {
	Authorize(ctx context.Context) error
}

// Config is /var/lib/edge/noded.json, written by first boot from instance metadata.
type Config struct {
	AllowUsers []string `json:"allow_users"` // Tailscale login names
	AllowTags  []string `json:"allow_tags"`  // Tailscale ACL tags, e.g. "tag:admin"
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil // empty allowlist: everyone refused
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// WhoIs is what we need from tailscaled's local API.
type WhoIs struct {
	LoginName string
	Tags      []string
}

// Lookup resolves a peer address to its Tailscale identity.
type Lookup func(ctx context.Context, remote string) (WhoIs, error)

type tailscale struct {
	cfg    Config
	lookup Lookup
}

func NewTailscale(cfg Config, lookup Lookup) Authorizer { return &tailscale{cfg: cfg, lookup: lookup} }

func (t *tailscale) Authorize(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return status.Error(codes.PermissionDenied, "no peer address")
	}
	if len(t.cfg.AllowUsers) == 0 && len(t.cfg.AllowTags) == 0 {
		return status.Error(codes.PermissionDenied, "no callers are allowed on this node")
	}
	who, err := t.lookup(ctx, p.Addr.String())
	if err != nil {
		return status.Errorf(codes.PermissionDenied, "cannot identify peer: %v", err)
	}
	for _, u := range t.cfg.AllowUsers {
		if who.LoginName != "" && strings.EqualFold(u, who.LoginName) {
			return nil
		}
	}
	for _, want := range t.cfg.AllowTags {
		for _, have := range who.Tags {
			if want == have {
				return nil
			}
		}
	}
	return status.Error(codes.PermissionDenied, "peer is not on the allowlist")
}

// LocalAPILookup asks tailscaled over its unix socket.
func LocalAPILookup(socket string) Lookup {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	return func(ctx context.Context, remote string) (WhoIs, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/whois?addr="+remote, nil)
		if err != nil {
			return WhoIs{}, err
		}
		req.Header.Set("Host", "local-tailscaled.sock")
		resp, err := client.Do(req)
		if err != nil {
			return WhoIs{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return WhoIs{}, fmt.Errorf("whois: %s", resp.Status)
		}
		var r struct {
			UserProfile struct{ LoginName string }
			Node        struct{ Tags []string }
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return WhoIs{}, err
		}
		return WhoIs{LoginName: r.UserProfile.LoginName, Tags: r.Node.Tags}, nil
	}
}
