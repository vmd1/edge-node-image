package nodectl

import (
	"strings"
	"testing"
)

func TestRolloutOrderCanaryFirstAndRegionsAlternate(t *testing.T) {
	nodes := []string{"ares-01", "ares-02", "hera-01", "hera-02", "zeus-01", "zeus-02"}
	got := rolloutOrder(nodes, "hera-02")
	if got[0] != "hera-02" || len(got) != 6 {
		t.Fatalf("%v", got)
	}
	for i := 1; i < len(got); i++ {
		if region(got[i]) == region(got[i-1]) {
			t.Fatalf("consecutive nodes share a region: %v", got)
		}
	}
	seen := map[string]bool{}
	for _, n := range got {
		if seen[n] {
			t.Fatalf("duplicate: %v", got)
		}
		seen[n] = true
	}
}

func TestRolloutOrderUnevenRegions(t *testing.T) {
	got := rolloutOrder([]string{"a-1", "a-2", "a-3", "b-1"}, "b-1")
	if len(got) != 4 || got[0] != "b-1" {
		t.Fatalf("%v", got)
	}
	// only one region has nodes left after the canary: they must still all be scheduled
	if strings.Join(got[1:], ",") != "a-1,a-2,a-3" {
		t.Fatalf("%v", got)
	}
}

func TestRolloutOrderWithoutCanaryInFleet(t *testing.T) {
	got := rolloutOrder([]string{"x-1", "y-1"}, "missing-1")
	if len(got) != 2 {
		t.Fatalf("%v", got)
	}
}
