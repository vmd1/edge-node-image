package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKubectl puts a script named kubectl first on PATH that prints the given text.
func fakeKubectl(t *testing.T, out string, exit int) {
	t.Helper()
	d := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + out + "\nEOF\nexit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(filepath.Join(d, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d+":"+os.Getenv("PATH"))
}

func TestOtherIngressProblemsWarnsButIgnoresSelf(t *testing.T) {
	fakeKubectl(t, "zeus-01 False true\nzeus-02 True \nares-01 False \nhera-01 True true", 0)
	got := otherIngressProblems(context.Background(), "zeus-01")
	joined := strings.Join(got, "; ")
	if strings.Contains(joined, "zeus-01") {
		t.Errorf("must not warn about the node being updated: %v", got)
	}
	if !strings.Contains(joined, "ares-01 is not Ready") || !strings.Contains(joined, "hera-01 is cordoned") {
		t.Errorf("missing warnings: %v", got)
	}
	if strings.Contains(joined, "zeus-02") {
		t.Errorf("healthy node flagged: %v", got)
	}
}

func TestOtherIngressProblemsWhenKubectlFails(t *testing.T) {
	fakeKubectl(t, "boom", 1)
	got := otherIngressProblems(context.Background(), "zeus-01")
	if len(got) != 1 || !strings.Contains(got[0], "could not list") {
		t.Errorf("a kubectl failure must be reported as a warning, not hidden: %v", got)
	}
}

func TestPlanText(t *testing.T) {
	if orNewest("") != "the newest version" || orNewest("2.0.0") != "2.0.0" {
		t.Error("orNewest")
	}
	if kubePlan(&env{noKube: true}) != "" || kubePlan(&env{}) != "cordon+drain, " {
		t.Error("kubePlan")
	}
}

func TestNeed(t *testing.T) {
	if need([]string{"a"}, 2, "msg") == nil || need([]string{"a", "b"}, 2, "msg") != nil {
		t.Error("need")
	}
}

func TestRunUnknownCommandAndMissingNode(t *testing.T) {
	e := &env{addr: "127.0.0.1:1"}
	if err := run(context.Background(), e, []string{"status"}); err == nil {
		t.Error("status without a node must fail")
	}
	if err := run(context.Background(), e, []string{"frobnicate", "node"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("unknown command: %v", err)
	}
}
