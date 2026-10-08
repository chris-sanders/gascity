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

func decodedAgentCommandChunks(t *testing.T, args []string, firstChunk int, cfg runtime.Config) []string {
	t.Helper()
	if len(args) <= firstChunk {
		t.Fatal("agent command was not passed as chunks")
	}
	chunks := args[firstChunk:]
	if len(chunks) == 0 {
		t.Fatal("agent command was not passed as chunks")
	}
	for i, chunk := range chunks {
		if len(chunk) > 32*1024 {
			t.Fatalf("agent command chunk %d is %d bytes, want at most 32 KiB", i, len(chunk))
		}
	}
	encoded := strings.Join(chunks, "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode agent command chunks: %v", err)
	}
	if got, want := string(decoded), decodedAgentCommand(t, cfg); got != want {
		t.Fatalf("reconstructed command differs: got %d bytes, want %d", len(got), len(want))
	}
	return chunks
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
	startArgs := pod.Spec.Containers[0].Args
	startCommand := startArgs[0]
	startChunks := decodedAgentCommandChunks(t, startArgs, 2, cfg)

	if err := p.Relaunch(context.Background(), name, cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	respawn := findExecCmd(fake, "respawn-pane")
	if respawn == nil {
		t.Fatal("Relaunch did not issue respawn-pane")
	}
	relaunchCommand := respawn[2]
	relaunchChunks := decodedAgentCommandChunks(t, respawn, 4, cfg)
	if !reflect.DeepEqual(startChunks, relaunchChunks) {
		t.Fatal("Start and Relaunch passed different command chunks")
	}
	for name, script := range map[string]string{"Start": startCommand, "Relaunch": relaunchCommand} {
		if strings.Contains(script, agentCommandB64(cfg)) {
			t.Errorf("%s embedded the base64 command in its shell script", name)
		}
		if !strings.Contains(script, `printf '%s' "$@" | base64 -d`) {
			t.Errorf("%s does not reconstruct the command from positional chunks", name)
		}
	}
}

func TestAgentCommandChunksStayBelowLinuxArgumentLimit(t *testing.T) {
	prompt := strings.Repeat("p", 99*1024)
	cfg := runtime.Config{
		Command:      "codex --quiet",
		PromptSuffix: shellquote.Quote(prompt),
	}
	encoded := agentCommandB64(cfg)
	if len(encoded) <= 128*1024 {
		t.Fatalf("fixture base64 command is %d bytes, want over 128 KiB", len(encoded))
	}

	pod, err := buildPod("large-prompt", cfg, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	chunks := decodedAgentCommandChunks(t, pod.Spec.Containers[0].Args, 2, cfg)
	if len(chunks) < 2 {
		t.Fatalf("large command used %d chunks, want multiple", len(chunks))
	}
	if got := strings.Join(chunks, ""); got != encoded {
		t.Fatalf("joined chunks differ from encoded command: got %d bytes, want %d", len(got), len(encoded))
	}
}

func TestDynamicUserPromptMetacharactersStayInChunkPayload(t *testing.T) {
	prompt := "single ' quote; double \" quote; $HOME; $(printf substituted); `printf backtick`; semicolon; backslash \\; newline\nsecond line"
	cfg := runtime.Config{
		Command:      "codex --quiet",
		PromptSuffix: shellquote.Quote(prompt),
		WorkDir:      "/city/rigs/prompt agent",
		Env: map[string]string{
			"GC_AGENT":       "prompt-agent",
			"GC_CITY":        "/city",
			"LINUX_USERNAME": "gcagent",
		},
	}
	wantArgv := []string{"codex", "--quiet", prompt}
	if got := decodedAgentArgv(t, cfg); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("decoded agent argv = %#v, want hostile prompt as one literal arg", got)
	}

	startScript := buildAgentLaunchCommand(cfg, "tmux new-session -d -s "+tmuxSession, true)
	startArgs := agentCommandShellArgs(startScript, cfg)
	decodedAgentCommandChunks(t, startArgs, 2, cfg)

	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "prompt-agent", SanitizeName("prompt-agent"))
	hasSessionAlive(fake, SanitizeName("prompt-agent"))
	if err := p.Relaunch(context.Background(), "prompt-agent", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	relaunchArgs := findExecCmd(fake, "respawn-pane")
	if relaunchArgs == nil {
		t.Fatal("Relaunch did not issue respawn-pane")
	}
	relaunchScript := relaunchArgs[2]
	decodedAgentCommandChunks(t, relaunchArgs, 4, cfg)

	for name, script := range map[string]string{"Start": startScript, "Relaunch": relaunchScript} {
		if strings.Contains(script, prompt) || strings.Contains(script, agentCommandB64(cfg)) {
			t.Errorf("%s interpolated prompt data into its shell script", name)
		}
		marker := `printf '%s' "$@" | base64 -d | su - ` + shellquote.Quote("gcagent") + ` -c `
		if !strings.Contains(script, marker) {
			t.Errorf("%s does not pipe the encoded chunks into a static su command", name)
			continue
		}
		suCommand := script[strings.Index(script, marker)+len(`printf '%s' "$@" | base64 -d | `):]
		suArgs := shellquote.Split(suCommand)
		if len(suArgs) < 5 {
			t.Errorf("%s su command = %#v, want a static -c script", name, suArgs)
			continue
		}
		userScript := suArgs[4]
		if !strings.Contains(userScript, "CMD=$(base64 -d)") {
			t.Errorf("%s inner su script does not decode the command from stdin", name)
		}
		if !strings.Contains(userScript, `cd `+shellquote.Quote(projectedPodWorkDir(cfg))+` &&`) {
			t.Errorf("%s inner su script does not shell-quote the projected work directory", name)
		}
		if !strings.Contains(userScript, `"$CMD"`) {
			t.Errorf("%s does not pass the decoded command as one quoted tmux argument", name)
		}
	}
}
