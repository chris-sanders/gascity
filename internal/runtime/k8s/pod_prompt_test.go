package k8s

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func decodedAgentCommand(t *testing.T, cfg runtime.Config) string {
	t.Helper()

	command, err := base64.StdEncoding.DecodeString(agentCommandB64(cfg))
	if err != nil {
		t.Fatalf("decode agent command: %v", err)
	}
	return string(command)
}

func decodedAgentArgv(t *testing.T, cfg runtime.Config) []string {
	t.Helper()
	return shellquote.Split(decodedAgentCommand(t, cfg))
}

func TestAgentCommandIncludesPromptAsPositionalArg(t *testing.T) {
	prompt := `Start with spaces, "quotes", and $HOME intact.`
	cfg := runtime.Config{
		Command:      "codex --quiet",
		PromptSuffix: shellquote.Quote(prompt),
	}

	want := []string{"codex", "--quiet", prompt}
	if got := decodedAgentArgv(t, cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("agent argv = %#v, want %#v", got, want)
	}
}

func TestAgentCommandIncludesPromptFlagBeforePrompt(t *testing.T) {
	prompt := "Start the configured task now."
	cfg := runtime.Config{
		Command:      "codex --quiet",
		PromptFlag:   "--prompt",
		PromptSuffix: shellquote.Quote(prompt),
	}

	want := []string{"codex", "--quiet", "--prompt", prompt}
	if got := decodedAgentArgv(t, cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("agent argv = %#v, want %#v", got, want)
	}
}

func TestAgentCommandWithoutPromptSuffixPreservesCommand(t *testing.T) {
	cfg := runtime.Config{
		Command:    "codex --quiet --color never",
		PromptFlag: "--prompt",
	}

	if got, want := decodedAgentCommand(t, cfg), cfg.Command; got != want {
		t.Fatalf("agent command = %q, want unchanged command %q", got, want)
	}
}

func TestAgentCommandRemapsPathsAfterAppendingPrompt(t *testing.T) {
	prompt := "Read /city/packs/gascity/brief.md before starting."
	cfg := runtime.Config{
		Command:      "codex --add-dir /city/rigs/gascity",
		PromptSuffix: shellquote.Quote(prompt),
		Env:          map[string]string{"GC_CITY": "/city"},
	}

	want := []string{"codex", "--add-dir", "/workspace/rigs/gascity", "Read /workspace/packs/gascity/brief.md before starting."}
	if got := decodedAgentArgv(t, cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("agent argv = %#v, want %#v", got, want)
	}
}

func TestAgentCommandOmitsNudgePromptsFromArgv(t *testing.T) {
	oversized := strings.Repeat("oversized-startup-prompt-", 5000)
	for _, tc := range []struct {
		name       string
		prompt     string
		promptFlag string
	}{
		{name: "none mode", prompt: "deliver this startup prompt through nudge"},
		{name: "oversized arg fallback", prompt: oversized},
		{name: "oversized flag fallback", prompt: oversized, promptFlag: "--prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runtime.Config{
				Command:    "codex --quiet",
				PromptFlag: tc.promptFlag,
				Nudge:      tc.prompt,
			}

			command := decodedAgentCommand(t, cfg)
			if got, want := command, cfg.Command; got != want {
				t.Fatalf("agent command = %q, want only the base command %q", got, want)
			}
			if strings.Contains(command, tc.prompt) {
				t.Fatal("nudge prompt leaked into the agent command")
			}
		})
	}
}

func TestStartAndRelaunchUseSamePromptCommand(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	p.postStartSettle = 0

	name := "prompt-agent"
	podName := SanitizeName(name)
	prompt := "Use the argv startup prompt on both launches."
	cfg := runtime.Config{
		Command:      "codex --quiet",
		PromptFlag:   "--prompt",
		PromptSuffix: shellquote.Quote(prompt),
		Env:          map[string]string{"GC_AGENT": "prompt-agent"},
	}
	fake.setExecResult(podName, []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)

	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pod := fake.pods[podName]
	if pod == nil {
		t.Fatal("Start did not create the agent pod")
	}
	startCommand := pod.Spec.Containers[0].Args[0]

	if err := p.Relaunch(context.Background(), name, cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	respawn := findExecCmd(fake, "respawn-pane")
	if respawn == nil {
		t.Fatal("Relaunch did not issue respawn-pane")
	}
	relaunchCommand := respawn[len(respawn)-1]

	encoded := "echo '" + agentCommandB64(cfg) + "' | base64 -d"
	if !strings.Contains(startCommand, encoded) {
		t.Fatalf("Start command does not contain reconstructed prompt command: %s", startCommand)
	}
	if !strings.Contains(relaunchCommand, encoded) {
		t.Fatalf("Relaunch command does not contain the same reconstructed prompt command: %s", relaunchCommand)
	}
}
