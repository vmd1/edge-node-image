// Package auth decides which peers may call noded.
//
// There is no identity or allowlist: who may reach noded is decided by the Tailscale ACLs (noded only listens on the node's
// Tailscale address). The one check done here is about where the connection comes from: it must arrive from another address
// in the tailnet range. Connections from pods (cluster addresses), from loopback, and from the node itself (host-network pods
// and any local process connecting to the node's own Tailscale address) are refused, so a compromised workload on the node
// cannot use the ACL-open port.
package auth

import (
	"context"
	"net"
	"net/netip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Authorizer reports whether the caller behind ctx may use the API.
type Authorizer interface {
	Authorize(ctx context.Context) error
}

var tailnetRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),       // Tailscale IPv4 (CGNAT range)
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"), // Tailscale IPv6
}

type tailnetOnly struct{ localAddrs func() ([]netip.Addr, error) }

// NewTailnetOnly returns the authorizer described in the package comment.
func NewTailnetOnly() Authorizer { return &tailnetOnly{localAddrs: interfaceAddrs} }

// NewTailnetOnlyWithLocal is for tests: it takes the node's own addresses explicitly.
func NewTailnetOnlyWithLocal(local []netip.Addr) Authorizer {
	return &tailnetOnly{localAddrs: func() ([]netip.Addr, error) { return local, nil }}
}

func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

func (t *tailnetOnly) Authorize(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return status.Error(codes.PermissionDenied, "no peer address")
	}
	ap, err := netip.ParseAddrPort(p.Addr.String())
	if err != nil {
		return status.Error(codes.PermissionDenied, "unrecognised peer address")
	}
	ip := ap.Addr().Unmap()
	inTailnet := false
	for _, r := range tailnetRanges {
		if r.Contains(ip) {
			inTailnet = true
			break
		}
	}
	if !inTailnet {
		return status.Error(codes.PermissionDenied, "callers must come from the tailnet")
	}
	local, err := t.localAddrs()
	if err != nil {
		return status.Error(codes.PermissionDenied, "cannot determine the node's own addresses")
	}
	for _, l := range local {
		if l == ip {
			return status.Error(codes.PermissionDenied, "callers on this node are not allowed")
		}
	}
	return nil
}
