package server

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/auth"
	"github.com/vmd1/edge-node-image/internal/config"
)

// methodRoles is the least role each RPC needs. A method missing from the table needs admin (fail closed).
var methodRoles = map[string]auth.Role{
	"Status": auth.RoleReader, "Logs": auth.RoleReader, "CheckUpdate": auth.RoleReader, "GetUpdateConfig": auth.RoleReader,
	"GetRegistryConfig": auth.RoleReader, "GetConfig": auth.RoleReader, "ValidateConfig": auth.RoleReader, "Inspect": auth.RoleReader,
	"ListFiles": auth.RoleReader, "Health": auth.RoleReader, "Events": auth.RoleReader, "WhoAmI": auth.RoleReader,
	"ReadFile": auth.RoleOperator, "Support": auth.RoleOperator, "Pcap": auth.RoleOperator,
	"StageUpdate": auth.RoleOperator, "Reboot": auth.RoleOperator, "SetBootSlot": auth.RoleOperator,
	"SetConfigOverride": auth.RoleOperator, "ClearConfigOverride": auth.RoleOperator,
	"SetUpdateConfig": auth.RoleAdmin, "SetRegistryConfig": auth.RoleAdmin,
	// Service: status is a read, start/stop/restart need operator (see RequiredRole)
	"Service": auth.RoleOperator, "Reset": auth.RoleOperator, "ResetPreflight": auth.RoleReader,
}

func baseName(full string) string {
	for i := len(full) - 1; i >= 0; i-- {
		if full[i] == '/' {
			return full[i+1:]
		}
	}
	return full
}

// RequiredRole returns the role a call needs; req may be nil for streams.
func RequiredRole(fullMethod string, req any) auth.Role {
	name := baseName(fullMethod)
	if name == "Service" {
		if r, ok := req.(*pb.ServiceRequest); ok && r.Action == "status" {
			return auth.RoleReader
		}
	}
	if name == "Reset" {
		if r, ok := req.(*pb.ResetRequest); ok && r.Wipe == "all-var" {
			return auth.RoleAdmin // a factory reset beats operator
		}
	}
	if r, ok := methodRoles[name]; ok {
		return r
	}
	return auth.RoleAdmin
}

// PolicyInterceptors applies the tailnet check, then identifies the caller and applies the role policy and audit log.
func PolicyInterceptors(a auth.Authorizer, p *auth.Policy) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := a.Authorize(ctx); err != nil {
				return nil, err
			}
			ctx, done, err := p.Check(ctx, baseName(info.FullMethod), RequiredRole(info.FullMethod, req))
			if err != nil {
				return nil, err
			}
			resp, err := h(ctx, req)
			done(err)
			return resp, err
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := a.Authorize(ss.Context()); err != nil {
				return err
			}
			ctx, done, err := p.Check(ss.Context(), baseName(info.FullMethod), RequiredRole(info.FullMethod, nil))
			if err != nil {
				return err
			}
			err = h(srv, &wrapped{ServerStream: ss, ctx: ctx})
			done(err)
			return err
		}),
	}
}

type wrapped struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrapped) Context() context.Context { return w.ctx }

// ModeFromConfig reads apiRoles from the applied config (cached for 5 s). An absent or unreadable config means audit.
func ModeFromConfig(varDir string) func() auth.Mode {
	var mu sync.Mutex
	var at time.Time
	cur := auth.ModeAudit
	return func() auth.Mode {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < 5*time.Second {
			return cur
		}
		at = time.Now()
		cur = auth.ModeAudit
		sv := config.Store{Path: varDir + "/config/saved.json"}.Load()
		if sv.Applied != nil && sv.Applied.APIRoles == "enforce" {
			cur = auth.ModeEnforce
		}
		if os.Getenv("NODED_ROLES") == "enforce" {
			cur = auth.ModeEnforce
		}
		return cur
	}
}

func NewPolicy(sock, varDir string) *auth.Policy {
	return &auth.Policy{Whois: auth.LocalAPIWhois{Socket: sock}, Mode: ModeFromConfig(varDir), Logf: log.Printf}
}

func (s *Server) WhoAmI(ctx context.Context, _ *pb.WhoAmIRequest) (*pb.WhoAmIResponse, error) {
	id, _ := auth.FromContext(ctx)
	mode := auth.ModeAudit
	if s.Mode != nil {
		mode = s.Mode()
	}
	return &pb.WhoAmIResponse{Device: id.Device, User: id.User, Role: id.Role.String(), Mode: string(mode)}, nil
}
