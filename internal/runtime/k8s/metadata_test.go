package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

type tmuxMetadataTestOps struct {
	*fakeK8sOps
	tmuxBinary string
	tmuxSocket string
	tmuxPath   string
}

func (o *tmuxMetadataTestOps) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	var input []byte
	if stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		input = data
	}
	o.calls = append(o.calls, fakeCall{method: "execInPod", pod: pod, container: container, cmd: cmd, stdin: input})
	binary := cmd[0]
	command := cmd[1:]
	if binary == "tmux" {
		binary = o.tmuxPath
	}
	process := exec.CommandContext(ctx, binary, command...)
	process.Env = append(os.Environ(),
		"PATH="+filepath.Dir(o.tmuxPath)+":"+os.Getenv("PATH"),
		"GC_TEST_TMUX_BIN="+o.tmuxBinary,
		"GC_TEST_TMUX_SOCKET="+o.tmuxSocket,
	)
	if stdin != nil {
		process.Stdin = strings.NewReader(string(input))
	}
	output, err := process.CombinedOutput()
	if err == nil {
		return string(output), nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		status := exitError.ExitCode()
		return string(output), execerr.CodeExitError{Err: fmt.Errorf("exit %d: %s", status, strings.TrimSpace(string(output))), Code: status}
	}
	return string(output), err
}

func newTmuxMetadataTestOps(t *testing.T, podName string) (*tmuxMetadataTestOps, func(...string) (string, error)) {
	t.Helper()
	tmuxBinary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is unavailable in this validation environment")
	}
	root := t.TempDir()
	socket := filepath.Join(root, "tmux.sock")
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmuxPath := filepath.Join(binDir, "tmux")
	wrapper := "#!/bin/sh\nexec \"$GC_TEST_TMUX_BIN\" -S \"$GC_TEST_TMUX_SOCKET\" \"$@\"\n"
	if err := os.WriteFile(tmuxPath, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		command := exec.Command(tmuxBinary, append([]string{"-S", socket}, args...)...)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if _, err := run("new-session", "-d", "-s", "metadata-seed", "sleep", "300"); err != nil {
		t.Fatalf("start tmux test server: %v", err)
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	fake := newFakeK8sOps()
	addRunningPod(fake, podName, podName)
	return &tmuxMetadataTestOps{fakeK8sOps: fake, tmuxBinary: tmuxBinary, tmuxSocket: socket, tmuxPath: tmuxPath}, run
}

func TestSeedStartMetadataUsesAllowlistAndSecureSourceFile(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "pod-a", "worker-a")
	env := map[string]string{
		"GC_SESSION_ID":       "session-current",
		"GC_RUNTIME_EPOCH":    "epoch-current",
		"GC_INSTANCE_TOKEN":   "instance-capability",
		"GC_PROVIDER":         "codex",
		"GC_CITY_PATH":        "/workspace/city",
		"ANTHROPIC_API_KEY":   "ordinary-secret",
		"UNCLASSIFIED_SECRET": "ordinary-secret",
		"PATH":                "/attacker/path",
	}

	if err := p.seedStartMetadata(context.Background(), "pod-a", "worker-a", env); err != nil {
		t.Fatalf("seedStartMetadata: %v", err)
	}
	call := findSourceFileCall(t, fake.calls)
	commands := string(call.stdin)
	if strings.Contains(strings.Join(call.cmd, " "), "session-current") || strings.Contains(strings.Join(call.cmd, " "), "instance-capability") {
		t.Fatal("identity values appeared in exec argv")
	}
	for _, value := range []string{"ordinary-secret", "/attacker/path"} {
		if strings.Contains(commands, value) || strings.Contains(strings.Join(call.cmd, " "), value) {
			t.Fatalf("ordinary environment value %q was seeded", value)
		}
	}
	if !strings.Contains(commands, "'-r' 'GC_SESSION_ID'") || !strings.Contains(commands, "'BEADS_HOLDER_TOKEN' 'instance-capability'") {
		t.Fatalf("identity removals or holder derivation missing from source file: %s", commands)
	}
	if strings.LastIndex(commands, "'GC_SESSION_ID' 'session-current'") < strings.LastIndex(commands, "'GC_CITY_PATH' '/workspace/city'") {
		t.Fatal("session ID was not the last identity publication")
	}
	for _, key := range protocolMetadataKeys {
		if !strings.Contains(commands, "'-r' '"+key+"'") {
			t.Errorf("new generation did not clear shared protocol key %s", key)
		}
	}
	for _, required := range []string{"umask 077", "flock -x -w 10", "mktemp -d", "set -C; cat >", "chmod 600", "tmux source-file"} {
		if !strings.Contains(call.cmd[2], required) {
			t.Errorf("secure source command missing %q", required)
		}
	}
}

func TestTmuxRemovalMarkerShadowsGlobalIdentityEnvironment(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is unavailable in this validation environment")
	}
	socket := filepath.Join(t.TempDir(), "tmux.sock")
	run := func(args ...string) (string, error) {
		output, err := exec.Command(tmux, append([]string{"-S", socket}, args...)...).CombinedOutput()
		return string(output), err
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	if _, err := run("new-session", "-d", "-s", "metadata-seed"); err != nil {
		t.Fatalf("start tmux server: %v", err)
	}
	if _, err := run("set-environment", "-g", "GC_SESSION_ID", "stale-global-session"); err != nil {
		t.Fatalf("set global identity: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession); err != nil {
		t.Fatalf("new tmux session: %v", err)
	}
	if _, err := run("kill-session", "-t", "metadata-seed"); err != nil {
		t.Fatalf("remove seed session: %v", err)
	}
	if got, err := run("show-environment", "-g", "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "GC_SESSION_ID=stale-global-session" {
		t.Fatalf("global identity = %q, %v; want the stale global value", got, err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "-r", "GC_SESSION_ID"); err != nil {
		t.Fatalf("remove session identity: %v", err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "-GC_SESSION_ID" {
		t.Fatalf("removed identity = %q, %v; want tmux removal marker that shadows global value", got, err)
	}
	value := "quoted ' value\nsecond line"
	commandsPath := filepath.Join(t.TempDir(), "commands.tmux")
	commands := tmuxMetadataCommand("set-environment", "-t", tmuxSession, "GC_META_TEST", value) + "\n"
	if err := os.WriteFile(commandsPath, []byte(commands), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("source-file", commandsPath); err != nil {
		t.Fatalf("source metadata with quote and newline: %v", err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_META_TEST"); err != nil || strings.TrimSuffix(got, "\n") != "GC_META_TEST="+value {
		t.Fatalf("tmux parsed metadata = %q, %v; want exact quoted multiline value", got, err)
	}
}

func TestMetadataSourceFailureDoesNotExposeIdentity(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "pod-a", "worker-a")
	secret := "synthetic-sensitive-capability"
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if strings.Contains(strings.Join(cmd, " "), "tmux source-file") {
			return "", errors.New("exec failed with " + secret)
		}
		return "", nil
	}
	err := p.seedStartMetadata(context.Background(), "pod-a", "worker-a", map[string]string{
		"GC_SESSION_ID":     "synthetic-session-id",
		"GC_INSTANCE_TOKEN": secret,
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("seed error = %v; expected a redacted failure", err)
	}
	call := findSourceFileCall(t, fake.calls)
	if strings.Contains(strings.Join(call.cmd, " "), secret) {
		t.Fatal("identity capability appeared in argv")
	}
}

func TestMetadataProtocolRoundTripKeepsOneTmuxAuthority(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")
	metadata := map[string]string{}
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "show-environment") {
			key := cmd[len(cmd)-1]
			if strings.Contains(joined, " -g ") {
				return "-" + key + "\n", nil
			}
			if value, ok := metadata[key]; ok {
				return key + "=" + value + "\n", nil
			}
			if strings.Contains(joined, "has-session") {
				return "", nil
			}
		}
		return "", nil
	}

	for key, value := range map[string]string{
		"GC_DRAIN": "1", "GC_DRAIN_ACK_SOURCE": "agent",
		"GC_DRAIN_REASON": "restart", "GC_DRAIN_GENERATION": "42",
		"GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN": "requester-token",
		"GC_RESTART_REQUESTED":                  "1", "GC_DRIFT_RESTART": "1",
	} {
		if err := p.SetMeta("worker", key, value); err != nil {
			t.Fatalf("controller SetMeta(%s): %v", key, err)
		}
		metadata[key] = value
	}
	// The in-pod agent uses the same tmux environment authority to publish ACK.
	if err := p.SetMeta("worker", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("agent ACK SetMeta: %v", err)
	}
	metadata["GC_DRAIN_ACK"] = "1"
	for key, want := range map[string]string{
		"GC_DRAIN": "1", "GC_DRAIN_ACK_SOURCE": "agent",
		"GC_DRAIN_REASON": "restart", "GC_DRAIN_GENERATION": "42",
		"GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN": "requester-token",
		"GC_RESTART_REQUESTED":                  "1", "GC_DRIFT_RESTART": "1", "GC_DRAIN_ACK": "1",
	} {
		got, err := p.GetMeta("worker", key)
		if err != nil || got != want {
			t.Fatalf("GetMeta(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	if err := p.RemoveMeta("worker", "GC_DRAIN_ACK"); err != nil {
		t.Fatalf("controller clear ACK: %v", err)
	}
	call := lastSourceFileCall(t, fake.calls)
	if !strings.Contains(string(call.stdin), "'-r' 'GC_DRAIN_ACK'") {
		t.Fatalf("protocol clear did not use tmux removal marker: %s", call.stdin)
	}
}

func TestK8sMetadataProtocolAgentInteropAndGenerationRecycle(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	if _, err := run("set-environment", "-g", "GC_DRAIN", "stale-global-drain"); err != nil {
		t.Fatalf("set stale global protocol flag: %v", err)
	}
	if _, err := run("set-environment", "-g", "GC_SESSION_ID", "stale-global-session"); err != nil {
		t.Fatalf("set stale global session ID: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create protocol session: %v", err)
	}
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	if err := p.seedStartMetadata(context.Background(), "worker", "worker", map[string]string{
		"GC_SESSION_ID": "session-before", "GC_INSTANCE_TOKEN": "instance-before", "GC_PROVIDER": "claude",
	}); err != nil {
		t.Fatalf("seed initial identity: %v", err)
	}

	controllerFlags := map[string]string{
		"GC_DRAIN": "operator", "GC_DRAIN_REASON": "handoff", "GC_DRAIN_GENERATION": "42",
		"GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN": "requester-instance",
		"GC_RESTART_REQUESTED":                  "1700000000", "GC_DRIFT_RESTART": "1",
	}
	for key, value := range controllerFlags {
		if err := p.SetMeta("worker", key, value); err != nil {
			t.Fatalf("controller SetMeta(%s): %v", key, err)
		}
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_DRAIN"); err != nil || strings.TrimSpace(got) != "GC_DRAIN=operator" {
		t.Fatalf("in-pod drain check = %q, %v; want controller's drain request", got, err)
	}
	for key, value := range map[string]string{"GC_DRAIN_ACK": "1", "GC_DRAIN_ACK_SOURCE": "agent"} {
		if _, err := run("set-environment", "-t", tmuxSession, key, value); err != nil {
			t.Fatalf("in-pod agent SetMeta(%s): %v", key, err)
		}
	}
	for key, want := range map[string]string{
		"GC_DRAIN": "operator", "GC_DRAIN_ACK": "1", "GC_DRAIN_ACK_SOURCE": "agent",
		"GC_DRAIN_REASON": "handoff", "GC_DRAIN_GENERATION": "42",
		"GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN": "requester-instance",
		"GC_RESTART_REQUESTED":                  "1700000000", "GC_DRIFT_RESTART": "1",
	} {
		got, err := p.GetMeta("worker", key)
		if err != nil || got != want {
			t.Fatalf("controller GetMeta(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range protocolMetadataKeys {
		if err := p.RemoveMeta("worker", key); err != nil {
			t.Fatalf("controller RemoveMeta(%s): %v", key, err)
		}
		if got, err := run("show-environment", "-t", tmuxSession, key); err != nil || strings.TrimSpace(got) != "-"+key {
			t.Fatalf("agent-visible %s after controller clear = %q, %v; want removal marker", key, got, err)
		}
	}
	if got, err := run("show-environment", "-g", "GC_DRAIN"); err != nil || strings.TrimSpace(got) != "GC_DRAIN=stale-global-drain" {
		t.Fatalf("global drain test fixture = %q, %v; want stale global value to remain", got, err)
	}
	if got, err := run("show-environment", "-g", "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "GC_SESSION_ID=stale-global-session" {
		t.Fatalf("global identity test fixture = %q, %v; want stale global value to remain", got, err)
	}

	if err := p.seedStartMetadata(context.Background(), "worker", "worker", map[string]string{
		"GC_SESSION_ID": "session-after", "GC_INSTANCE_TOKEN": "instance-after", "GC_PROVIDER": "codex",
	}); err != nil {
		t.Fatalf("seed replacement generation: %v", err)
	}
	for key, want := range map[string]string{"GC_SESSION_ID": "session-after", "GC_INSTANCE_TOKEN": "instance-after", "BEADS_HOLDER_TOKEN": "instance-after", "GC_PROVIDER": "codex"} {
		if got, err := p.GetMeta("worker", key); err != nil || got != want {
			t.Fatalf("replacement identity %s = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range protocolMetadataKeys {
		if got, err := p.GetMeta("worker", key); err != nil || got != "" {
			t.Fatalf("replacement generation retained %s=%q: %v", key, got, err)
		}
	}
	if err := p.seedStartMetadata(context.Background(), "worker", "worker", map[string]string{"GC_INSTANCE_TOKEN": "instance-without-session-id"}); err != nil {
		t.Fatalf("seed generation with absent session ID: %v", err)
	}
	if got, err := p.GetMeta("worker", "GC_SESSION_ID"); err != nil || got != "" {
		t.Fatalf("absent identity was restored from the global environment: %q, %v", got, err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "-GC_SESSION_ID" {
		t.Fatalf("absent session identity marker = %q, %v; want global-shadowing removal", got, err)
	}
}

func TestK8sMetadataBackfillsLegacyIdentityAndRuntimeOwnerFromPodEnv(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "legacy")
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	for key, value := range map[string]string{
		"GC_SESSION_ID": "legacy-session", "GC_INSTANCE_TOKEN": "legacy-instance",
		"GC_PROVIDER": "claude", "GC_CITY_PATH": "/legacy/city", "GT_PROCESS_NAMES": "claude,codex",
	} {
		t.Setenv(key, value)
	}
	if got, err := p.GetMeta("legacy", "GC_SESSION_ID"); err != nil || got != "legacy-session" {
		t.Fatalf("legacy session identity backfill = %q, %v", got, err)
	}
	if got, err := p.GetMeta("legacy", "BEADS_HOLDER_TOKEN"); err != nil || got != "legacy-instance" {
		t.Fatalf("legacy holder token backfill = %q, %v; want instance token", got, err)
	}
	for key, want := range map[string]string{"GC_CITY_PATH": "/legacy/city", "GT_PROCESS_NAMES": "claude,codex"} {
		if got, err := p.GetMeta("legacy", key); err != nil || got != want {
			t.Fatalf("legacy runtime owner %s backfill = %q, %v; want %q", key, got, err, want)
		}
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "GC_SESSION_ID=legacy-session" {
		t.Fatalf("backfilled identity in tmux = %q, %v", got, err)
	}
}

func TestControllerPrivateMetadataIsDurableAndOutsidePod(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	ctx := context.Background()
	if err := p.setPrivateMetadata(ctx, "worker/with/path", "delivery_checkpoint", "typed"); err != nil {
		t.Fatalf("set private metadata: %v", err)
	}
	got, err := p.getPrivateMetadata(ctx, "worker/with/path", "delivery_checkpoint")
	if err != nil || got != "typed" {
		t.Fatalf("get private metadata = %q, %v", got, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("controller-private metadata touched pod exec: %+v", fake.calls)
	}
	files, err := filepath.Glob(filepath.Join(city, ".gc", "runtime-meta", "k8s", "*-delivery_checkpoint"))
	if err != nil || len(files) != 1 {
		t.Fatalf("controller metadata files = %v, %v", files, err)
	}
	info, err := os.Stat(files[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("controller metadata mode = %v, %v; want 0600", info.Mode(), err)
	}
}

func TestRelaunchFailureLeavesControllerIdentityUnbound(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")
	p.postStartSettle = 0
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "tmux-metadata.lock") && strings.Contains(joined, "respawn-pane") {
			return "", errors.New("transport failed after dispatch")
		}
		return "", nil
	}
	err := p.Relaunch(context.Background(), "worker", runtime.Config{
		Command: "agent --resume",
		Env: map[string]string{
			"GC_SESSION_ID": "next-session", "GC_RUNTIME_EPOCH": "next-epoch", "GC_INSTANCE_TOKEN": "next-token",
		},
	})
	if err == nil {
		t.Fatal("Relaunch succeeded after an ambiguous respawn")
	}
	if got, getErr := p.getPrivateMetadata(context.Background(), "worker", identityUnboundKey); getErr != nil || got != "1" {
		t.Fatalf("identity fence after failed relaunch = %q, %v; want 1", got, getErr)
	}
	if got, getErr := p.getMetaFromPod(context.Background(), "worker", "worker", "GC_SESSION_ID"); getErr != nil || got != "" {
		t.Fatalf("identity read after failed relaunch = %q, %v; want hidden", got, getErr)
	}
}

func TestRelaunchPublishesIdentityAfterReplacementPaneBind(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")
	p.postStartSettle = 0
	fake.execFunc = func(_ string, _ []string) (string, error) { return "", nil }
	if err := p.Relaunch(context.Background(), "worker", runtime.Config{
		Command: "agent --resume",
		Env: map[string]string{
			"GC_SESSION_ID": "new-session", "GC_RUNTIME_EPOCH": "next-epoch", "GC_INSTANCE_TOKEN": "next-token",
		},
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	respawnIndex, publishIndex := -1, -1
	for i, call := range fake.calls {
		joined := strings.Join(call.cmd, " ")
		if call.method != "execInPod" {
			continue
		}
		if respawnIndex < 0 && strings.Contains(joined, "tmux respawn-pane") {
			respawnIndex = i
			if !strings.Contains(joined, "identity.bound") || !strings.Contains(joined, `[ -f "$bound" ]`) {
				t.Fatal("identity publication wait does not require the replacement pane's bound marker")
			}
			if strings.Contains(joined, "new-session") || strings.Contains(joined, "next-token") || strings.Contains(joined, "next-epoch") {
				t.Fatal("new identity value appeared in respawn argv")
			}
			if !strings.Contains(string(call.stdin), "GC_SESSION_ID='new-session'") || !strings.Contains(string(call.stdin), "GC_INSTANCE_TOKEN='next-token'") {
				t.Fatalf("replacement identity was not staged through stdin: %q", call.stdin)
			}
		}
		if strings.Contains(joined, "tmux source-file") && strings.Contains(string(call.stdin), "next-token") {
			publishIndex = i
		}
	}
	if respawnIndex < 0 || publishIndex <= respawnIndex {
		t.Fatalf("identity publication index=%d, replacement respawn index=%d; want publication after bind", publishIndex, respawnIndex)
	}
}

func TestK8sMetadataAbsentAndTransportErrorsAreDistinct(t *testing.T) {
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "worker", "worker")

	missing := execerr.CodeExitError{Err: fmt.Errorf("command exited with status 1"), Code: 1}
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "show-environment") && strings.Contains(joined, "GC_DRAIN_ACK") {
			return "", missing
		}
		if strings.Contains(joined, "has-session") {
			return "", nil
		}
		return "", nil
	}
	if got, err := p.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || got != "" {
		t.Fatalf("GetMeta(absent) = %q, %v; want empty, nil after session check", got, err)
	}
	transportErr := errors.New("transport unavailable")
	fake.execFunc = func(_ string, cmd []string) (string, error) {
		if strings.Contains(strings.Join(cmd, " "), "show-environment") {
			return "", transportErr
		}
		return "", nil
	}
	if _, err := p.GetMeta("worker", "GC_DRAIN"); !errors.Is(err, transportErr) {
		t.Fatalf("GetMeta(transport error) = %v; want wrapped transport error", err)
	}
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

func findSourceFileCall(t *testing.T, calls []fakeCall) fakeCall {
	t.Helper()
	for _, call := range calls {
		if call.method == "execInPod" && strings.Contains(strings.Join(call.cmd, " "), "tmux source-file") {
			return call
		}
	}
	t.Fatal("tmux source-file exec was not recorded")
	return fakeCall{}
}

func lastSourceFileCall(t *testing.T, calls []fakeCall) fakeCall {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].method == "execInPod" && strings.Contains(strings.Join(calls[i].cmd, " "), "tmux source-file") {
			return calls[i]
		}
	}
	t.Fatal("tmux source-file exec was not recorded")
	return fakeCall{}
}
