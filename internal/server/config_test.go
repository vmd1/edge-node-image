package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
)

func TestConfigValidateOverrideClear(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()

	v, _ := s.ValidateConfig(ctx, &pb.ValidateConfigRequest{SpecJson: `{"kubelet":{"maxPods":60}}`})
	if !v.Valid || !strings.Contains(v.Rendered, "max-pods=60") {
		t.Fatalf("%+v", v)
	}
	for _, bad := range []string{`{"kubelet":{"maxPods":3}}`, `{"nope":1}`, `{"sysctls":{"kernel.core_pattern":"1"}}`, `not json`} {
		if v, _ := s.ValidateConfig(ctx, &pb.ValidateConfigRequest{SpecJson: bad}); v.Valid || v.Error == "" {
			t.Errorf("accepted %s", bad)
		}
	}

	r, err := s.SetConfigOverride(ctx, &pb.SetConfigOverrideRequest{SpecJson: `{"sysctls":{"vm.swappiness":"60"}}`, TtlSeconds: 600})
	if err != nil || r.Expires == "" {
		t.Fatalf("%v %+v", err, r)
	}
	st, _ := s.GetConfig(ctx, &pb.GetConfigRequest{})
	if !strings.Contains(st.OverrideJson, "vm.swappiness") || st.OverrideExpires == "" {
		t.Fatalf("%+v", st)
	}
	if _, err := s.SetConfigOverride(ctx, &pb.SetConfigOverrideRequest{SpecJson: `{"mode":"bad"}`}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid override must be refused: %v", err)
	}
	if _, err := s.SetConfigOverride(ctx, &pb.SetConfigOverrideRequest{SpecJson: `{}`, TtlSeconds: 9 * 24 * 3600}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ttl over 7d must be refused: %v", err)
	}
	if _, err := s.ClearConfigOverride(ctx, &pb.ClearConfigOverrideRequest{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.GetConfig(ctx, &pb.GetConfigRequest{}); st.OverrideJson != "" {
		t.Fatalf("override not cleared: %+v", st)
	}
}
