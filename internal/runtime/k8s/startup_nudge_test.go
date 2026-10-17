package k8s

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStartupNudgeIntentIsControllerPrivateAndDiagnosticOmitsMessage(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.cityPath = t.TempDir()
	cfg := runtime.Config{
		ProviderName: "claude",
		Nudge:        "  start the task\nwith care  ",
		Env: map[string]string{
			"GC_SESSION_ID":     "session-1",
			"GC_INSTANCE_TOKEN": "instance-1",
			"GC_RUNTIME_EPOCH":  "4",
			"GC_PROVIDER":       "claude",
		},
	}
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", cfg)
	if err != nil {
		t.Fatalf("persistStartupNudgeIntent: %v", err)
	}
	if intent.Message != cfg.Nudge {
		t.Fatalf("intent message = %q, want exact configured bytes %q", intent.Message, cfg.Nudge)
	}
	root := filepath.Join(p.cityPath, ".gc", "runtime-meta", "k8s")
	intentPath := controllerMetadataValuePath(root, "worker", startupNudgeIntentKey(intent.ID))
	statePath := controllerMetadataValuePath(root, "worker", startupNudgeStateKey(intent.ID))
	for _, path := range []string{intentPath, statePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(path), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", filepath.Base(path), got)
		}
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("controller metadata root mode = %04o, want 0700", got)
	}
	var stored startupNudgeIntent
	data, err := os.ReadFile(intentPath)
	if err != nil || json.Unmarshal(data, &stored) != nil {
		t.Fatalf("reading immutable intent: %v", err)
	}
	if stored.MessageDigest != intent.MessageDigest || stored.ProviderFamily != "claude" {
		t.Fatalf("stored immutable intent = %+v", stored)
	}
	diagnostics, err := StartupNudgeDiagnostics(p.cityPath)
	if err != nil {
		t.Fatalf("StartupNudgeDiagnostics: %v", err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Status != startupNudgeUnsent {
		t.Fatalf("diagnostics = %+v, want one definitely-unsent intent", diagnostics)
	}
	targets := p.StartupNudgeLeaseTargets(context.Background())
	if len(targets) != 1 || targets[0].SessionID != "session-1" {
		t.Fatalf("lease targets = %+v, want the intent's session row ID", targets)
	}
	if strings.Contains(strings.Join([]string{diagnostics[0].Reason, diagnostics[0].SessionName, diagnostics[0].IntentID}, " "), cfg.Nudge) {
		t.Fatal("startup nudge diagnostic exposed prompt content")
	}
}

func TestStartupNudgeRecoveryMarksTypedDraftUncertainWithoutTransportRetry(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.cityPath = t.TempDir()
	cfg := runtime.Config{ProviderName: "claude", Nudge: "run task", Env: map[string]string{"GC_SESSION_ID": "session-1", "GC_INSTANCE_TOKEN": "instance-1", "GC_PROVIDER": "claude"}}
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.updateStartupNudgeState(context.Background(), intent, startupNudgeState{Status: startupNudgeTyped, Progress: "1/1"}); err != nil {
		t.Fatal(err)
	}
	statuses := p.RecoverStartupNudgesForSession(context.Background(), "worker")
	if len(statuses) != 1 || statuses[0].Status != startupNudgeUncertain || !statuses[0].Changed {
		t.Fatalf("recovery statuses = %+v, want one durable uncertain transition", statuses)
	}
	for _, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) > 1 && call.cmd[0] == "tmux" && call.cmd[1] == "send-keys" {
			t.Fatalf("recovery submitted an ambiguous typed draft: %+v", call)
		}
	}
	state, err := p.loadStartupNudgeState(context.Background(), intent)
	if err != nil || state.Status != startupNudgeUncertain || !state.Reported {
		t.Fatalf("checkpoint = %+v, %v; want reported uncertainty", state, err)
	}
}

func TestStartupNudgeRecoveryRejectsIntentFromDifferentLeasedSessionRow(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.cityPath = t.TempDir()
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", runtime.Config{
		ProviderName: "claude",
		Nudge:        "do not send into a replacement session",
		Env: map[string]string{
			"GC_SESSION_ID":     "old-session-id",
			"GC_INSTANCE_TOKEN": "old-instance-token",
			"GC_PROVIDER":       "claude",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	statuses := p.RecoverStartupNudgesForLeasedSession(context.Background(), "worker", "new-session-id")
	if len(statuses) != 1 || statuses[0].Status != startupNudgeUncertain || statuses[0].Reason != "session_row_incarnation_mismatch" {
		t.Fatalf("statuses = %+v, want a persisted row-incarnation mismatch", statuses)
	}
	state, err := p.loadStartupNudgeState(context.Background(), intent)
	if err != nil || state.Status != startupNudgeUncertain || state.Reason != "session_row_incarnation_mismatch" {
		t.Fatalf("state = %+v, %v; want durable uncertainty", state, err)
	}
	for _, call := range fake.calls {
		if call.method == "execInPod" {
			t.Fatalf("stale intent touched the runtime before matching the leased session row: %+v", call)
		}
	}
}

func TestInspectStartupComposerRequiresExactVisibleDraftAndCursor(t *testing.T) {
	geometry := startupComposerGeometry{Width: 80, Height: 4, CursorX: len([]rune("❯ run task")), CursorY: 2}
	if ok, reason := inspectStartupComposer("claude", "run task", "output\n\n❯ run task\n", geometry); !ok {
		t.Fatalf("exact Claude composer rejected: %s", reason)
	}

	cases := []struct {
		name, provider, message, screen string
		geometry                        startupComposerGeometry
	}{
		{"transcript echo", "claude", "run task", "run task\n\n❯ another draft\n", geometry},
		{"suffix", "claude", "run task", "output\n\n❯ run task old\n", geometry},
		{"wrong cursor", "claude", "run task", "output\n\n❯ run task\n", startupComposerGeometry{Width: 80, Height: 4, CursorX: 8, CursorY: 2}},
		{"outside prompt region", "claude", "run task", "❯ run task\n", startupComposerGeometry{Width: 80, Height: 4, CursorX: 10, CursorY: 1}},
		{"wrapped draft", "codex", "run task", "output\n\n› run task\n", startupComposerGeometry{Width: 8, Height: 4, CursorX: 10, CursorY: 2}},
		{"multiline intent", "claude", "run\ntask", "output\n\n❯ run\ntask\n", geometry},
		{"unknown provider", "other", "run task", "output\n\n❯ run task\n", geometry},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if ok, _ := inspectStartupComposer(test.provider, test.message, test.screen, test.geometry); ok {
				t.Fatal("composer inspection accepted an ambiguous layout")
			}
		})
	}
	if !hasStartupComposerModal("Permission request: Allow once?") {
		t.Fatal("composer modal detector did not reject a permission confirmation")
	}
}

func TestStartupNudgeRecoveryResumesOnlyExactRecognizedComposer(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "worker", "worker")
	fake.pods["worker"].UID = types.UID("pod-generation-1")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "show-environment" {
			key := cmd[len(cmd)-1]
			if len(cmd) > 2 && cmd[2] == "-g" {
				return "-" + key + "\n", nil
			}
			values := map[string]string{
				"GC_SESSION_ID":     "session-1",
				"GC_INSTANCE_TOKEN": "instance-1",
				"GC_PROVIDER":       "claude",
			}
			if value, ok := values[key]; ok {
				return key + "=" + value + "\n", nil
			}
			return "-" + key + "\n", nil
		}
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "display-message" {
			if len(cmd) > 4 && cmd[len(cmd)-1] == "#{pane_pid}" {
				return "4242\n", nil
			}
			return "80 4 10 2\n", nil
		}
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "capture-pane" {
			return "output\n\n❯ run task\n", nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	p.cityPath = t.TempDir()
	cfg := runtime.Config{ProviderName: "claude", Nudge: "run task", Env: map[string]string{"GC_SESSION_ID": "session-1", "GC_INSTANCE_TOKEN": "instance-1", "GC_PROVIDER": "claude"}}
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.updateStartupNudgeState(context.Background(), intent, startupNudgeState{
		Status: startupNudgeTyped, Progress: "1/1", PodName: "worker", PodUID: "pod-generation-1",
		ContainerID: "containerd://test-worker", PanePID: "4242",
	}); err != nil {
		t.Fatal(err)
	}
	if err := resumeStartupNudgeSubmission(context.Background(), p, intent, startupNudgeState{
		Status: startupNudgeTyped, Progress: "1/1", PodName: "worker", PodUID: "pod-generation-1",
		ContainerID: "containerd://test-worker", PanePID: "4242",
	}); err != nil {
		t.Fatalf("resumeStartupNudgeSubmission: %v", err)
	}
	state, err := p.loadStartupNudgeState(context.Background(), intent)
	if err != nil || state.Status != startupNudgeSubmitted {
		t.Fatalf("state = %+v, %v; want transport submitted", state, err)
	}
	var submitCount int
	for _, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) > 1 && call.cmd[0] == "tmux" && call.cmd[1] == "send-keys" {
			submitCount++
		}
	}
	if submitCount != 1 {
		t.Fatalf("submit transport count = %d, want exactly one Enter", submitCount)
	}
}

func TestStartupNudgeOperatorResolutionIsIncarnationFenced(t *testing.T) {
	city := t.TempDir()
	p := newProviderWithOps(newFakeK8sOps())
	p.cityPath = city
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", runtime.Config{
		ProviderName: "claude",
		Nudge:        "immutable kickoff",
		Env:          map[string]string{"GC_SESSION_ID": "session-1", "GC_INSTANCE_TOKEN": "instance-1", "GC_PROVIDER": "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.updateStartupNudgeState(context.Background(), intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "submit_result_ambiguous"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveStartupNudge(city, intent.ID, "wrong-incarnation", "retry"); err == nil {
		t.Fatal("ResolveStartupNudge accepted a different incarnation")
	}
	status, err := ResolveStartupNudge(city, intent.ID, "instance-1", "retry")
	if err != nil || status != startupNudgeUnsent {
		t.Fatalf("explicit retry = %q, %v; want definitely_unsent", status, err)
	}
	state, err := p.loadStartupNudgeState(context.Background(), intent)
	if err != nil || state.OperatorAction != "retry" || state.OperatorIncarnation != "instance-1" || state.Status != startupNudgeUnsent {
		t.Fatalf("retry checkpoint = %+v, %v", state, err)
	}
	status, err = ResolveStartupNudge(city, intent.ID, "instance-1", "delivered")
	if err != nil || status != startupNudgeOperatorConfirmed {
		t.Fatalf("delivery confirmation = %q, %v; want operator_confirmed", status, err)
	}
	diagnostics, err := StartupNudgeDiagnostics(city)
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("diagnostics after explicit resolution = %+v, %v; want none", diagnostics, err)
	}
}

func TestStartupNudgeDeliveryUsesPersistedProviderAndFencesPaneGeneration(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "worker", "worker")
	fake.pods["worker"].UID = types.UID("pod-generation-1")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "show-environment" {
			key := cmd[len(cmd)-1]
			if len(cmd) >= 3 && cmd[2] == "-g" {
				return "-" + key + "\n", nil
			}
			values := map[string]string{
				"GC_SESSION_ID":     "session-1",
				"GC_INSTANCE_TOKEN": "instance-1",
				"GC_PROVIDER":       "claude",
			}
			if value, ok := values[key]; ok {
				return key + "=" + value + "\n", nil
			}
			return "-" + key + "\n", nil
		}
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "display-message" {
			return "4242\n", nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	p.cityPath = t.TempDir()
	cfg := runtime.Config{
		ProviderName: "codex",
		Nudge:        "  exact startup prompt  ",
		Env: map[string]string{
			"GC_SESSION_ID":     "session-1",
			"GC_INSTANCE_TOKEN": "instance-1",
			"GC_PROVIDER":       "claude",
		},
	}
	intent, err := p.persistStartupNudgeIntent(context.Background(), "worker", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if intent.ProviderFamily != "claude" {
		t.Fatalf("persisted provider = %q, want env-selected claude", intent.ProviderFamily)
	}
	if err := p.deliverStartupNudge(context.Background(), intent); err != nil {
		t.Fatalf("deliverStartupNudge: %v", err)
	}
	state, err := p.loadStartupNudgeState(context.Background(), intent)
	if err != nil || state.Status != startupNudgeSubmitted {
		t.Fatalf("checkpoint = %+v, %v; want transport submitted", state, err)
	}
	recovered := p.RecoverStartupNudgesForSession(context.Background(), "worker")
	if len(recovered) != 1 || recovered[0].Status != startupNudgeSubmitted || !recovered[0].Changed {
		t.Fatalf("first submitted-state scan = %+v; want one durable transport-submitted event", recovered)
	}
	if recovered = p.RecoverStartupNudgesForSession(context.Background(), "worker"); len(recovered) != 0 {
		t.Fatalf("second submitted-state scan = %+v; want no duplicate event", recovered)
	}
	var sendCommands [][]string
	for _, call := range fake.calls {
		if call.method == "execInPod" && len(call.cmd) > 1 && call.cmd[0] == "tmux" && call.cmd[1] == "send-keys" {
			sendCommands = append(sendCommands, call.cmd)
		}
	}
	if len(sendCommands) != 2 || len(sendCommands[0]) != 6 || sendCommands[0][5] != "  exact startup prompt  " || strings.Join(sendCommands[1], " ") != "tmux send-keys -t main Enter" {
		t.Fatalf("send commands = %v, want one literal paste and one Claude Enter", sendCommands)
	}
}

func TestStartupNudgeSubmitSequenceIsProviderSpecific(t *testing.T) {
	tests := []struct {
		provider string
		want     []string
		ok       bool
	}{
		{provider: "claude", want: []string{"Enter"}, ok: true},
		{provider: "anthropic", want: []string{"Enter"}, ok: true},
		{provider: "codex", want: []string{"Escape", "Enter"}, ok: true},
		{provider: "unknown", ok: false},
	}
	for _, test := range tests {
		got, ok := startupNudgeSubmitKeys(test.provider)
		if ok != test.ok || strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Errorf("startupNudgeSubmitKeys(%q) = %v, %t; want %v, %t", test.provider, got, ok, test.want, test.ok)
		}
	}
}

func TestStartupNudgePaneFenceIncludesContainerRestartGeneration(t *testing.T) {
	fake := newFakeK8sOps()
	addRunningPod(fake, "worker", "worker")
	fake.pods["worker"].UID = types.UID("pod-generation-1")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[0] == "tmux" && cmd[1] == "display-message" {
			return "4242\n", nil
		}
		return "", nil
	}
	p := newProviderWithOps(fake)
	if err := p.verifyStartupNudgePane(context.Background(), "worker", "main", "pod-generation-1", "containerd://test-worker", 0, "4242"); err != nil {
		t.Fatalf("current pane generation rejected: %v", err)
	}
	fake.pods["worker"].Status.ContainerStatuses[0].RestartCount = 1
	if err := p.verifyStartupNudgePane(context.Background(), "worker", "main", "pod-generation-1", "containerd://test-worker", 0, "4242"); err == nil {
		t.Fatal("pane fence accepted a restarted agent container")
	}
}
