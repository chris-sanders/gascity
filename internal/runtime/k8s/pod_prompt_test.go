package k8s

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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

func decodedRespawnAgentCommand(t *testing.T, args []string, cfg runtime.Config) string {
	t.Helper()
	commandArgs := args
	if len(args) >= 5 && args[0] == "su" && args[1] == "-" {
		commandArgs = shellquote.Split(args[4])
	}
	marker := -1
	for i, arg := range commandArgs {
		if arg == agentCommandArg0 {
			marker = i
			break
		}
	}
	if marker < 0 || marker+1 >= len(commandArgs) {
		t.Fatalf("respawn command did not pass encoded command chunks: %v", args)
	}
	encoded := strings.Join(commandArgs[marker+1:], "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode respawn command chunks: %v", err)
	}
	want := "exec sh -c " + shellquote.Quote(decodedAgentCommand(t, cfg))
	if !strings.Contains(string(decoded), want) {
		t.Fatalf("respawn payload does not launch the configured agent command %q: %s", want, decoded)
	}
	return string(decoded)
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
	if len(startChunks) == 0 {
		t.Fatal("Start did not pass command chunks")
	}
	decodedRespawnAgentCommand(t, respawn, cfg)
	for name, script := range map[string]string{"Start": startCommand, "Relaunch": relaunchCommand} {
		if strings.Contains(script, agentCommandB64(cfg)) {
			t.Errorf("%s embedded the base64 command in its shell script", name)
		}
	}
	if !strings.Contains(strings.Join(respawn, " "), `CMD=$(printf '%s' "$@" | base64 -d)`) {
		t.Fatal("Relaunch does not reconstruct its wrapped command from positional chunks")
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

	pod, err := buildPod("prompt-agent", cfg, newProviderWithOps(newFakeK8sOps()))
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	startArgs := pod.Spec.Containers[0].Args
	startScript := startArgs[0]
	decodedAgentCommandChunks(t, startArgs, 2, cfg)

	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "prompt-agent", SanitizeName("prompt-agent"))
	fake.pods["prompt-agent"].Spec.Containers = []corev1.Container{{
		Name: "agent",
		Env:  []corev1.EnvVar{{Name: "LINUX_USERNAME", Value: "gcagent"}},
	}}
	hasSessionAlive(fake, SanitizeName("prompt-agent"))
	if err := p.Relaunch(context.Background(), "prompt-agent", cfg); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	relaunchArgs := findExecCmd(fake, "respawn-pane")
	if relaunchArgs == nil {
		t.Fatal("Relaunch did not issue respawn-pane")
	}
	if len(relaunchArgs) < 5 || relaunchArgs[0] != "su" || relaunchArgs[1] != "-" || relaunchArgs[2] != "gcagent" || relaunchArgs[3] != "-c" {
		t.Fatalf("Relaunch did not execute through the configured tmux owner: %v", relaunchArgs)
	}
	relaunchShellArgs := shellquote.Split(relaunchArgs[4])
	if len(relaunchShellArgs) < 3 {
		t.Fatalf("Relaunch owner command could not be decoded: %v", relaunchArgs)
	}
	relaunchScript := relaunchShellArgs[2]
	decodedRespawnAgentCommand(t, relaunchArgs, cfg)

	for name, script := range map[string]string{"Start": startScript, "Relaunch": strings.Join(relaunchShellArgs, " ")} {
		if strings.Contains(script, prompt) || strings.Contains(script, agentCommandB64(cfg)) {
			t.Errorf("%s interpolated prompt data into its shell script", name)
		}
	}
	if strings.Contains(relaunchScript, prompt) || strings.Contains(relaunchScript, agentCommandB64(cfg)) {
		t.Fatal("Relaunch interpolated prompt data into its outer shell script")
	}
	if !strings.Contains(strings.Join(relaunchShellArgs, " "), `cd `+shellquote.Quote(projectedPodWorkDir(cfg))+` &&`) {
		t.Errorf("Relaunch does not enter the shell-quoted projected work directory: %v", relaunchShellArgs)
	}
	if !strings.Contains(strings.Join(relaunchShellArgs, " "), `"$CMD"`) {
		t.Errorf("Relaunch does not pass the decoded command as one quoted tmux argument: %v", relaunchShellArgs)
	}
}

func TestDynamicUserWrapperPreservesCommand(t *testing.T) {
	cfg := runtime.Config{
		Command: "codex --quiet",
		WorkDir: "/workspace",
		Env: map[string]string{
			"LINUX_USERNAME": "gcagent",
		},
	}
	binDir := t.TempDir()
	commandFile := filepath.Join(t.TempDir(), "tmux-command")
	argcFile := filepath.Join(t.TempDir(), "tmux-argc")
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	writeExecutable("su", `#!/bin/sh
set -eu
if [ "$1" != "-" ] || [ "$2" != "gcagent" ] || [ "$3" != "-c" ]; then
	exit 90
fi
cd() { :; }
eval "$4"
`)
	writeExecutable("tmux", `#!/bin/sh
set -eu
printf '%s' "$#" > "$TMUX_ARGC_FILE"
last=
for arg do last=$arg; done
printf '%s' "$last" > "$TMUX_COMMAND_FILE"
`)

	for _, tc := range []struct {
		name        string
		tmuxCommand string
	}{
		{name: "Start", tmuxCommand: "tmux new-session -d -s main"},
		{name: "Relaunch", tmuxCommand: "tmux respawn-pane -k -t main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := buildAgentLaunchCommand(cfg, tc.tmuxCommand, false)
			shellArgs := agentCommandShellArgs(script, cfg)
			cmd := exec.Command("/bin/sh", append([]string{"-c"}, shellArgs...)...)
			cmd.Env = []string{
				"PATH=" + binDir + ":/usr/bin:/bin",
				"TMUX_ARGC_FILE=" + argcFile,
				"TMUX_COMMAND_FILE=" + commandFile,
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("run generated dynamic-user wrapper: %v\n%s", err, output)
			}
			gotCommand, err := os.ReadFile(commandFile)
			if err != nil {
				t.Fatalf("read tmux command: %v", err)
			}
			if got, want := string(gotCommand), "codex --quiet"; got != want {
				t.Fatalf("tmux command = %q, want %q", got, want)
			}
			gotArgc, err := os.ReadFile(argcFile)
			if err != nil {
				t.Fatalf("read tmux argument count: %v", err)
			}
			if got, want := string(gotArgc), "5"; got != want {
				t.Fatalf("tmux argument count = %q, want %q (command stays one argument)", got, want)
			}
		})
	}
}
