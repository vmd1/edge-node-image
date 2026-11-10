package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/vmd1/edge-node-image/gen/nodedv1"
	"github.com/vmd1/edge-node-image/internal/config"
)

func (s *Server) overrides() *config.Overrides {
	return &config.Overrides{Path: s.P.Var + "/config/override.json"}
}
func (s *Server) saved() config.Saved {
	return config.Store{Path: s.P.Var + "/config/saved.json"}.Load()
}

func decodeSpec(js string) (config.Spec, error) {
	var sp config.Spec
	d := json.NewDecoder(strings.NewReader(js))
	d.DisallowUnknownFields()
	if err := d.Decode(&sp); err != nil {
		return sp, fmt.Errorf("spec is not valid EdgeNodeConfig JSON: %w", err)
	}
	return sp, nil
}

func renderText(sp config.Spec) string {
	r := config.Render(sp)
	paths := make([]string, 0, len(r))
	for p := range r {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b bytes.Buffer
	for _, p := range paths {
		fmt.Fprintf(&b, "# %s\n%s\n", p, r[p])
	}
	return b.String()
}

func mustJSON(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Server) GetConfig(context.Context, *pb.GetConfigRequest) (*pb.ConfigState, error) {
	sv := s.saved()
	st := &pb.ConfigState{AppliedHash: sv.AppliedHash}
	var applied config.Spec
	if sv.Applied != nil {
		applied = *sv.Applied
		st.AppliedJson = mustJSON(sv.Applied)
	}
	st.Rendered = renderText(applied)
	if sv.Staged != nil {
		st.StagedJson = mustJSON(sv.Staged)
	}
	if o := s.overrides().Active(); o != nil {
		st.OverrideJson = mustJSON(o)
		if b, err := os.ReadFile(s.P.Var + "/config/override.json"); err == nil {
			var f struct{ Expires time.Time }
			if json.Unmarshal(b, &f) == nil {
				st.OverrideExpires = f.Expires.Format(time.RFC3339)
			}
		}
	}
	return st, nil
}

func (s *Server) ValidateConfig(_ context.Context, r *pb.ValidateConfigRequest) (*pb.ValidateConfigResponse, error) {
	sp, err := decodeSpec(r.SpecJson)
	if err == nil {
		err = sp.Validate()
	}
	if err != nil {
		return &pb.ValidateConfigResponse{Valid: false, Error: err.Error()}, nil
	}
	return &pb.ValidateConfigResponse{Valid: true, Hash: sp.Hash(), Rendered: renderText(sp)}, nil
}

func (s *Server) SetConfigOverride(_ context.Context, r *pb.SetConfigOverrideRequest) (*pb.SetConfigOverrideResponse, error) {
	sp, err := decodeSpec(r.SpecJson)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, err := s.overrides().Set(sp, time.Duration(r.TtlSeconds)*time.Second)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.SetConfigOverrideResponse{Expires: exp.Format(time.RFC3339)}, nil
}

func (s *Server) ClearConfigOverride(context.Context, *pb.ClearConfigOverrideRequest) (*pb.ClearConfigOverrideResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.overrides().Clear(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.ClearConfigOverrideResponse{}, nil
}
