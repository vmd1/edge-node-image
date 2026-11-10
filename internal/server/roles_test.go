package server

import (
	"testing"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/auth"
)

// Every RPC in the service must be classified: an unlisted method would silently need admin, and a new read-only RPC that
// someone forgot would be unusable for readers. This test fails when a method is added without a decision.
func TestEveryRPCHasAnExplicitRole(t *testing.T) {
	for _, m := range pb.Noded_ServiceDesc.Methods {
		if _, ok := methodRoles[m.MethodName]; !ok {
			t.Errorf("unary RPC %s has no entry in methodRoles", m.MethodName)
		}
	}
	for _, m := range pb.Noded_ServiceDesc.Streams {
		if _, ok := methodRoles[m.StreamName]; !ok {
			t.Errorf("stream RPC %s has no entry in methodRoles", m.StreamName)
		}
	}
}

func TestRequiredRoleRules(t *testing.T) {
	cases := []struct {
		method string
		req    any
		want   auth.Role
	}{
		{"/noded.v1.Noded/Status", nil, auth.RoleReader},
		{"/noded.v1.Noded/ReadFile", nil, auth.RoleOperator},
		{"/noded.v1.Noded/Pcap", nil, auth.RoleOperator},
		{"/noded.v1.Noded/Reboot", nil, auth.RoleOperator},
		{"/noded.v1.Noded/SetRegistryConfig", nil, auth.RoleAdmin},
		{"/noded.v1.Noded/Service", &pb.ServiceRequest{Action: "status"}, auth.RoleReader},
		{"/noded.v1.Noded/Service", &pb.ServiceRequest{Action: "restart"}, auth.RoleOperator},
		{"/noded.v1.Noded/BrandNewThing", nil, auth.RoleAdmin},
	}
	for _, c := range cases {
		if got := RequiredRole(c.method, c.req); got != c.want {
			t.Errorf("%s: got %s want %s", c.method, got, c.want)
		}
	}
}
