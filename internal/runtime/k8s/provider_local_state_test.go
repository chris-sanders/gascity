package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStartProjectsLocalStateBeforeBeadsAndWorkspaceReady(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	fake.setExecResult("gc-test-agent", []string{"tmux", "has-session", "-t", "main"}, "", nil)

	cfg := runtime.Config{
		Command:      "claude",
		WorkDir:      "/city",
		ProcessNames: []string{"claude"},
		Env: map[string]string{
			"GC_AGENT":            "rig/worker",
			"GC_CITY":             "/city",
			"GC_DOLT_PORT":        "31364",
			"GC_STORE_ROOT":       "/city",
			"GC_BEADS_SCOPE_ROOT": "/city",
			"GC_BEADS_PROJECT_ID": "rig-project",
			"GC_BEADS_PREFIX":     "rg",
			"GC_DOLT_DATABASE":    "bd_rig_project",
		},
	}
	if err := p.Start(context.Background(), "gc-test-agent", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}

	initIndex := findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) >= 3 && cmd[0] == "env" && cmd[1] == "GC_DOLT=skip" && cmd[2] == "gc"
	})
	projectIndex := findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) >= 3 && cmd[0] == "gc" && cmd[1] == "internal" && cmd[2] == "project-k8s-local-state"
	})
	beadsIndex := findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) >= 2 && cmd[0] == "sh" && cmd[1] == "-c" && strings.Contains(strings.Join(cmd, " "), "bd init --server")
	})
	readyIndex := findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) == 2 && cmd[0] == "touch" && cmd[1] == "/workspace/.gc-workspace-ready"
	})
	cleanupIndex := findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) == 3 && cmd[0] == "rm" && cmd[1] == "-rf" && cmd[2] == "/tmp/city-src"
	})
	for label, index := range map[string]int{
		"gc init":                initIndex,
		"local-state projection": projectIndex,
		"source cleanup":         cleanupIndex,
		"scoped beads init":      beadsIndex,
		"workspace ready":        readyIndex,
	} {
		if index < 0 {
			t.Fatalf("missing %s call; calls=%#v", label, fake.calls)
		}
	}
	if !(initIndex < projectIndex && projectIndex < cleanupIndex && cleanupIndex < beadsIndex && beadsIndex < readyIndex) {
		t.Fatalf("startup order = init=%d project=%d cleanup=%d beads=%d ready=%d; calls=%#v", initIndex, projectIndex, cleanupIndex, beadsIndex, readyIndex, fake.calls)
	}

	projection := fake.calls[projectIndex].cmd
	if got, want := strings.Join(projection, " "), "gc internal project-k8s-local-state --source-root /tmp/city-src --controller-city-root /city --dest-root /workspace"; got != want {
		t.Fatalf("projection argv = %q, want %q", got, want)
	}
}

func TestStartLocalStateProjectionFailureCleansUpBeforeBeads(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 3 && cmd[0] == "gc" && cmd[1] == "internal" && cmd[2] == "project-k8s-local-state" {
			return "", errors.New("projection failed")
		}
		return "", nil
	}

	cfg := runtime.Config{
		Command: "claude",
		WorkDir: "/city",
		Env: map[string]string{
			"GC_AGENT": "rig/worker",
			"GC_CITY":  "/city",
		},
	}
	err := p.Start(context.Background(), "gc-test-agent", cfg)
	if err == nil || !strings.Contains(err.Error(), "projecting City local state") {
		t.Fatalf("Start error = %v, want local-state projection failure", err)
	}
	if findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) >= 2 && cmd[0] == "sh" && cmd[1] == "-c"
	}) >= 0 {
		t.Fatalf("scoped Beads init ran after projection failure: %#v", fake.calls)
	}
	if findK8sExecCall(fake, func(cmd []string) bool {
		return len(cmd) == 2 && cmd[0] == "touch" && cmd[1] == "/workspace/.gc-workspace-ready"
	}) >= 0 {
		t.Fatalf("workspace-ready released after projection failure: %#v", fake.calls)
	}
	if findK8sCall(fake, "deletePod") < 0 {
		t.Fatalf("failed projection did not clean up pod: %#v", fake.calls)
	}
}

func findK8sExecCall(fake *fakeK8sOps, match func([]string) bool) int {
	for i, call := range fake.calls {
		if call.method == "execInPod" && match(call.cmd) {
			return i
		}
	}
	return -1
}

func findK8sCall(fake *fakeK8sOps, method string) int {
	for i, call := range fake.calls {
		if call.method == method {
			return i
		}
	}
	return -1
}
