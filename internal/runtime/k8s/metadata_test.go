package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSeedStartMetadataUsesRuntimeKeyOrderAndFiltersSecrets(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	env := map[string]string{
		"GC_SESSION_ID":       "synthetic-session-id",
		"GC_RUNTIME_EPOCH":    "synthetic-epoch",
		"GC_INSTANCE_TOKEN":   "synthetic-instance-capability",
		"GC_PROVIDER":         "codex",
		"ANTHROPIC_API_KEY":   "synthetic-secret",
		"UNCLASSIFIED_SECRET": "synthetic-secret",
	}

	if err := p.seedStartMetadata(context.Background(), "pod-a", "worker-a", env); err != nil {
		t.Fatalf("seedStartMetadata: %v", err)
	}

	var gotKeys []string
	for _, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) == 6 &&
			call.cmd[0] == "tmux" && call.cmd[1] == "set-environment" {
			gotKeys = append(gotKeys, call.cmd[4])
		}
	}
	wantKeys := []string{"GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH", "GC_SESSION_ID", "GC_PROVIDER"}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("projected metadata key count = %d, want %d", len(gotKeys), len(wantKeys))
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Errorf("projected metadata key %d = %q, want %q", i, gotKeys[i], wantKeys[i])
		}
	}
	for _, call := range fake.calls {
		if call.method == "execInPod" && (strings.Contains(strings.Join(call.cmd, " "), "ANTHROPIC_API_KEY") || strings.Contains(strings.Join(call.cmd, " "), "UNCLASSIFIED_SECRET")) {
			t.Fatal("secret-bearing environment key was projected into tmux metadata")
		}
	}
}

func TestSeedStartMetadataStopsOnProjectionFailureWithoutValueInError(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	secretValue := "synthetic-sensitive-capability"
	fake.setExecResult("pod-a",
		[]string{"tmux", "set-environment", "-t", tmuxSession, "GC_INSTANCE_TOKEN", secretValue},
		"", errors.New("exec failed"))

	err := p.seedStartMetadata(context.Background(), "pod-a", "worker-a", map[string]string{
		"GC_INSTANCE_TOKEN": secretValue,
		"GC_SESSION_ID":     "synthetic-session-id",
	})
	if err == nil {
		t.Fatal("seedStartMetadata succeeded after a projection failure")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatal("metadata projection error exposed a value")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("metadata projection calls = %d, want one call and no blind retry", len(fake.calls))
	}
}

func TestK8sGetMetaDistinguishesMissingKeysFromReadFailures(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")

	fake.setExecResult("worker", []string{"tmux", "show-environment", "-t", tmuxSession, "MISSING"}, "-MISSING\n", nil)
	if got, err := p.GetMeta("worker", "MISSING"); err != nil || got != "" {
		t.Fatalf("GetMeta(explicitly unset) = %q, %v; want empty, nil", got, err)
	}

	missingErr := execerr.CodeExitError{Err: fmt.Errorf("command exited with status 1"), Code: 1}
	fake.setExecResult("worker", []string{"tmux", "show-environment", "-t", tmuxSession, "ABSENT"}, "", missingErr)
	if got, err := p.GetMeta("worker", "ABSENT"); err != nil || got != "" {
		t.Fatalf("GetMeta(absent key) = %q, %v; want empty, nil after session verification", got, err)
	}

	readErr := errors.New("exec transport unavailable")
	fake.setExecResult("worker", []string{"tmux", "show-environment", "-t", tmuxSession, "GC_SESSION_ID"}, "", readErr)
	if _, err := p.GetMeta("worker", "GC_SESSION_ID"); !errors.Is(err, readErr) {
		t.Fatalf("GetMeta(read failure) error = %v, want wrapped transport error", err)
	}
	if !hasExecCommand(fake, []string{"tmux", "has-session", "-t", tmuxSession}) {
		t.Fatal("absent metadata key was accepted without verifying the tmux session")
	}
}

func TestStartProjectsIdentityBeforeReturning(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0
	if err := p.Start(context.Background(), "worker", runtime.Config{
		Command: "agent",
		Env: map[string]string{
			"GC_SESSION_ID":     "synthetic-session-id",
			"GC_RUNTIME_EPOCH":  "synthetic-epoch",
			"GC_INSTANCE_TOKEN": "synthetic-capability",
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := projectedMetadataKeys(fake.calls)
	want := []string{"GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH", "GC_SESSION_ID"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Start metadata keys = %v, want %v", got, want)
	}
}

func TestRelaunchProjectsIdentityBeforeRespawn(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")
	if err := p.Relaunch(context.Background(), "worker", runtime.Config{
		Command: "agent --resume",
		Env: map[string]string{
			"GC_SESSION_ID":     "synthetic-session-id",
			"GC_RUNTIME_EPOCH":  "synthetic-epoch",
			"GC_INSTANCE_TOKEN": "synthetic-capability",
		},
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	keys := projectedMetadataKeys(fake.calls)
	want := []string{"GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH", "GC_SESSION_ID"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("Relaunch metadata keys = %v, want %v", keys, want)
	}
	respawnAt := -1
	for i, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) > 1 && call.cmd[0] == "sh" && strings.Contains(strings.Join(call.cmd, " "), "respawn-pane") {
			respawnAt = i
			break
		}
	}
	lastMetadataAt := -1
	for i, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) == 6 && call.cmd[0] == "tmux" && call.cmd[1] == "set-environment" {
			lastMetadataAt = i
		}
	}
	if respawnAt < 0 || lastMetadataAt < 0 || lastMetadataAt > respawnAt {
		t.Fatalf("Relaunch metadata projection at %d must precede respawn at %d", lastMetadataAt, respawnAt)
	}
}

func projectedMetadataKeys(calls []fakeCall) []string {
	var keys []string
	for _, call := range calls {
		if call.method == "execInPod" && len(call.cmd) == 6 && call.cmd[0] == "tmux" && call.cmd[1] == "set-environment" {
			keys = append(keys, call.cmd[4])
		}
	}
	return keys
}

func hasExecCommand(fake *fakeK8sOps, want []string) bool {
	for _, call := range fake.calls {
		if call.method != "execInPod" || len(call.cmd) != len(want) {
			continue
		}
		match := true
		for i := range want {
			match = match && call.cmd[i] == want[i]
		}
		if match {
			return true
		}
	}
	return false
}

func TestK8sMetaOperationsFailClosedWithoutRunningPod(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	if _, err := p.GetMeta("missing", "GC_SESSION_ID"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("GetMeta without pod = %v, want ErrSessionNotFound", err)
	}
	if err := p.SetMeta("missing", "GC_SESSION_ID", "synthetic-value"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SetMeta without pod = %v, want ErrSessionNotFound", err)
	}
	if err := p.RemoveMeta("missing", "GC_SESSION_ID"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("RemoveMeta without pod = %v, want ErrSessionNotFound", err)
	}
}

func TestK8sSetMetaDoesNotExposeValueWhenExecFails(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")
	secretValue := "synthetic-sensitive-capability"
	fake.setExecResult("worker",
		[]string{"tmux", "set-environment", "-t", tmuxSession, "GC_INSTANCE_TOKEN", secretValue},
		"", errors.New("synthetic command error"))

	err := p.SetMeta("worker", "GC_INSTANCE_TOKEN", secretValue)
	if err == nil {
		t.Fatal("SetMeta succeeded after an exec failure")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatal("SetMeta error exposed a metadata value")
	}
}
