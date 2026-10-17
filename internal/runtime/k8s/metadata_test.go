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
	tmuxEnv    string
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
	if o.tmuxEnv != "" {
		process.Env = append(process.Env, "TMUX="+o.tmuxEnv)
	}
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
	return newTmuxMetadataTestOpsWithRoute(t, podName, false)
}

func newTmuxMetadataTestOpsWithTMUX(t *testing.T, podName string) (*tmuxMetadataTestOps, func(...string) (string, error)) {
	return newTmuxMetadataTestOpsWithRoute(t, podName, true)
}

func newTmuxMetadataTestOpsWithRoute(t *testing.T, podName string, useTMUX bool) (*tmuxMetadataTestOps, func(...string) (string, error)) {
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
	if useTMUX {
		wrapper = "#!/bin/sh\nexec \"$GC_TEST_TMUX_BIN\" \"$@\"\n"
	}
	if err := os.WriteFile(tmuxPath, []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		command := exec.Command(tmuxBinary, append([]string{"-S", socket}, args...)...)
		command.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GC_TEST_TMUX_BIN="+tmuxBinary,
			"GC_TEST_TMUX_SOCKET="+socket,
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if _, err := run("new-session", "-d", "-s", "metadata-seed", "sleep", "300"); err != nil {
		t.Fatalf("start tmux test server: %v", err)
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	tmuxEnv := ""
	if useTMUX {
		server, err := run("display-message", "-p", "-t", "metadata-seed", "#{socket_path},#{pid},0")
		if err != nil {
			t.Fatalf("read tmux server environment: %v", err)
		}
		tmuxEnv = strings.TrimSpace(server)
		if !strings.HasPrefix(tmuxEnv, socket+",") {
			t.Fatalf("TMUX server environment = %q, want socket %q", tmuxEnv, socket)
		}
	}
	fake := newFakeK8sOps()
	addRunningPod(fake, podName, podName)
	return &tmuxMetadataTestOps{fakeK8sOps: fake, tmuxBinary: tmuxBinary, tmuxSocket: socket, tmuxPath: tmuxPath, tmuxEnv: tmuxEnv}, run
}

func tmuxMetadataPodExecs(calls []fakeCall) []fakeCall {
	var execs []fakeCall
	for _, call := range calls {
		if call.method == "execInPod" {
			execs = append(execs, call)
		}
	}
	return execs
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
	if !strings.Contains(commands, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", "GC_SESSION_ID")) ||
		!strings.Contains(commands, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "BEADS_HOLDER_TOKEN", "instance-capability")) {
		t.Fatalf("identity removals or holder derivation missing from source file: %s", commands)
	}
	if strings.LastIndex(commands, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "GC_SESSION_ID", "session-current")) <
		strings.LastIndex(commands, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "GC_CITY_PATH", "/workspace/city")) {
		t.Fatal("session ID was not the last identity publication")
	}
	for _, key := range protocolMetadataKeys {
		if !strings.Contains(commands, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", key)) {
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
	home := t.TempDir()
	run := func(args ...string) (string, error) {
		command := exec.Command(tmux, append([]string{"-f", "/dev/null", "-S", socket}, args...)...)
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TERM=xterm"}
		output, err := command.CombinedOutput()
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
	value := "quoted ' ; ${HOME} $ value\\tail"
	commandsPath := filepath.Join(t.TempDir(), "commands.tmux")
	commands := tmuxMetadataCommand("set-environment", "-t", tmuxSession, "GC_META_TEST", value) + "\n"
	if err := os.WriteFile(commandsPath, []byte(commands), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("source-file", commandsPath); err != nil {
		t.Fatalf("source metadata with tmux metacharacters: %v", err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_META_TEST"); err != nil || strings.TrimSuffix(got, "\n") != "GC_META_TEST="+value {
		t.Fatalf("tmux parsed metadata = %q, %v; source command = %q; want exact quoted value", got, err, commands)
	}
}

func TestSeedStartMetadataRejectsControlCharacters(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "pod-a", "worker-a")
	if err := p.seedStartMetadata(context.Background(), "pod-a", "worker-a", map[string]string{
		"GC_SESSION_ID": "session\nforged",
	}); err == nil {
		t.Fatal("seedStartMetadata accepted an identity value tmux cannot preserve")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("invalid identity reached pod exec: %+v", fake.calls)
	}
}

func TestValidateTmuxMetadataValueRejectsInvalidEncoding(t *testing.T) {
	if err := validateTmuxMetadataValue("GC_SESSION_NAME", string([]byte{0xff})); err == nil {
		t.Fatal("metadata value with invalid UTF-8 was accepted")
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
	if !strings.Contains(string(call.stdin), tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", "GC_DRAIN_ACK")) {
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
	if _, err := run("set-environment", "-g", "GC_RESTART_REQUESTED", "stale-global-restart"); err != nil {
		t.Fatalf("set stale global restart request: %v", err)
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
	if got, err := run("show-environment", "-t", tmuxSession, "GC_RESTART_REQUESTED"); err != nil || strings.TrimSpace(got) != "-GC_RESTART_REQUESTED" {
		t.Fatalf("initial generation inherited restart request = %q, %v; want removal marker", got, err)
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
	if got, err := run("list-panes", "-t", tmuxSession, "-F", "#{E:GC_RESTART_REQUESTED}"); err != nil || strings.TrimSpace(got) != "1700000000" {
		t.Fatalf("in-pod batched inventory marker = %q, %v; want controller marker", got, err)
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
	if got, err := run("list-panes", "-t", tmuxSession, "-F", "#{E:GC_RESTART_REQUESTED}"); err != nil || strings.TrimSpace(got) != "" {
		t.Fatalf("in-pod batched inventory after clear = %q, %v; want no stale restart marker", got, err)
	}
	if got, err := run("show-environment", "-g", "GC_DRAIN"); err != nil || strings.TrimSpace(got) != "GC_DRAIN=stale-global-drain" {
		t.Fatalf("global drain test fixture = %q, %v; want stale global value to remain", got, err)
	}
	if got, err := run("show-environment", "-g", "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "GC_SESSION_ID=stale-global-session" {
		t.Fatalf("global identity test fixture = %q, %v; want stale global value to remain", got, err)
	}
	if got, err := run("show-environment", "-g", "GC_RESTART_REQUESTED"); err != nil || strings.TrimSpace(got) != "GC_RESTART_REQUESTED=stale-global-restart" {
		t.Fatalf("global restart fixture = %q, %v; want stale global value to remain", got, err)
	}
	if err := p.seedStartMetadata(context.Background(), "worker", "worker", map[string]string{
		"GC_SESSION_ID": "session-after", "GC_INSTANCE_TOKEN": "instance-after", "GC_PROVIDER": "codex",
	}); err != nil {
		t.Fatalf("seed replacement generation: %v", err)
	}
	if got, err := run("list-panes", "-t", tmuxSession, "-F", "#{E:GC_RESTART_REQUESTED}"); err != nil || strings.TrimSpace(got) != "" {
		t.Fatalf("replacement generation restart marker = %q, %v; want no inherited value", got, err)
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

func TestK8sSessionIDSetClearsStaleHolderWithoutInstanceToken(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	if _, err := run("set-environment", "-g", "BEADS_HOLDER_TOKEN", "stale-global-holder"); err != nil {
		t.Fatalf("set stale global holder: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create metadata session: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "GC_INSTANCE_TOKEN", "old-instance"); err != nil {
		t.Fatalf("set old instance token: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "BEADS_HOLDER_TOKEN", "stale-session-holder"); err != nil {
		t.Fatalf("set stale session holder: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "-r", "GC_INSTANCE_TOKEN"); err != nil {
		t.Fatalf("remove instance token: %v", err)
	}
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	if err := p.SetMeta("worker", "GC_SESSION_ID", "replacement-session"); err != nil {
		t.Fatalf("set replacement session ID: %v", err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "BEADS_HOLDER_TOKEN"); err != nil || strings.TrimSpace(got) != "-BEADS_HOLDER_TOKEN" {
		t.Fatalf("holder after session transition = %q, %v; want removal marker", got, err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_SESSION_ID"); err != nil || strings.TrimSpace(got) != "GC_SESSION_ID=replacement-session" {
		t.Fatalf("session ID after transition = %q, %v", got, err)
	}
}

func TestK8sMetadataBackfillsLegacyIdentityAndRuntimeOwnerFromPodEnv(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "legacy")
	if _, err := run("set-environment", "-g", "GC_PROVIDER", "pod-provider"); err != nil {
		t.Fatalf("seed legacy global provider: %v", err)
	}
	if _, err := run("set-environment", "-g", "GC_ALIAS", "legacy-global-alias"); err != nil {
		t.Fatalf("seed legacy global alias: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create legacy session: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "GC_PROVIDER", "session-provider"); err != nil {
		t.Fatalf("seed session-local provider: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "-r", "GC_ALIAS"); err != nil {
		t.Fatalf("mark legacy alias explicitly removed: %v", err)
	}
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	for key, value := range map[string]string{
		"GC_SESSION_ID": "legacy-session", "GC_INSTANCE_TOKEN": "legacy-instance",
		"GC_RUNTIME_EPOCH": "legacy-runtime", "GC_CONTINUATION_EPOCH": "legacy-continuation",
		"GC_PROVIDER": "pod-provider", "GC_ALIAS": "pod-alias",
		"GC_CITY_PATH": "/legacy/city", "GT_PROCESS_NAMES": "claude,codex",
	} {
		t.Setenv(key, value)
	}
	t.Setenv("GC_TEMPLATE", "")
	ops.calls = nil
	if got, err := p.GetMeta("legacy", "GC_SESSION_ID"); err != nil || got != "legacy-session" {
		t.Fatalf("legacy session identity read = %q, %v; want backfilled value", got, err)
	}
	firstReadExecs := len(tmuxMetadataPodExecs(ops.calls))
	if current, err := p.hasCurrentIdentityMetadataVersion(context.Background(), "legacy"); err != nil || current {
		t.Fatalf("legacy identity version marker = %t, %v; want marker absent", current, err)
	}
	if marker, err := run("show-environment", "-h", "-t", tmuxSession, legacyIdentityBackfillKey); err != nil || strings.TrimSpace(marker) != legacyIdentityBackfillKey+"=1" {
		t.Fatalf("legacy compatibility completion marker = %q, %v", marker, err)
	}
	ops.calls = nil
	if got, err := p.GetMeta("legacy", "GC_PROVIDER"); err != nil || got != "session-provider" {
		t.Fatalf("second legacy identity read = %q, %v; want preserved session value", got, err)
	}
	secondExecs := tmuxMetadataPodExecs(ops.calls)
	for _, call := range secondExecs {
		joined := strings.Join(call.cmd, " ")
		if strings.Contains(joined, `printenv "$1"`) || strings.Contains(joined, "tmux source-file") {
			t.Fatalf("second identity read repeated compatibility repair exec: %q", call.cmd)
		}
	}
	if len(secondExecs) != 2 || len(secondExecs) >= firstReadExecs {
		t.Fatalf("second identity read used %d pod execs; want two reads without repair (first repair used %d)", len(secondExecs), firstReadExecs)
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
	if got, err := run("show-environment", "-t", tmuxSession, "GC_PROVIDER"); err != nil || strings.TrimSpace(got) != "GC_PROVIDER=session-provider" {
		t.Fatalf("backfill overwrote existing session identity = %q, %v", got, err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_ALIAS"); err != nil || strings.TrimSpace(got) != "-GC_ALIAS" {
		t.Fatalf("backfill overwrote explicit identity removal = %q, %v", got, err)
	}
	if _, err := run("show-environment", "-t", tmuxSession, "GC_TEMPLATE"); err == nil {
		t.Fatal("backfill created an identity value absent from the pod environment")
	}
	if current, err := p.hasCurrentIdentityMetadataVersion(context.Background(), "legacy"); err != nil || current {
		t.Fatalf("partial legacy backfill marker = %t, %v; want marker absent", current, err)
	}
}

func TestK8sLegacyBackfillGenerationGroupRequiresAllFiveMetadataKeysAbsent(t *testing.T) {
	podEnv := map[string]string{
		"GC_INSTANCE_TOKEN":     "pod-instance",
		"BEADS_HOLDER_TOKEN":    "pod-holder",
		"GC_RUNTIME_EPOCH":      "pod-runtime",
		"GC_CONTINUATION_EPOCH": "pod-continuation",
		"GC_SESSION_ID":         "pod-session",
	}
	tests := []struct {
		name      string
		seedKey   string
		seedValue string
		removed   bool
	}{
		{name: "continuation value survives", seedKey: "GC_CONTINUATION_EPOCH", seedValue: "session-continuation"},
		{name: "continuation removal survives", seedKey: "GC_CONTINUATION_EPOCH", removed: true},
		{name: "holder value fences generation", seedKey: "BEADS_HOLDER_TOKEN", seedValue: "adopted-holder"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, run := newTmuxMetadataTestOps(t, "worker")
			if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
				t.Fatalf("create legacy tmux session: %v", err)
			}
			if tt.removed {
				if _, err := run("set-environment", "-t", tmuxSession, "-r", tt.seedKey); err != nil {
					t.Fatalf("mark %s explicitly removed: %v", tt.seedKey, err)
				}
			} else if _, err := run("set-environment", "-t", tmuxSession, tt.seedKey, tt.seedValue); err != nil {
				t.Fatalf("seed session-local %s: %v", tt.seedKey, err)
			}
			for key, value := range podEnv {
				t.Setenv(key, value)
			}
			t.Setenv("GC_PROVIDER", "pod-provider")
			p := newProviderWithOps(ops)
			p.cityPath = t.TempDir()
			if got, err := p.GetMeta("worker", "GC_PROVIDER"); err != nil || got != "pod-provider" {
				t.Fatalf("descriptive identity backfill = %q, %v; want pod-provider", got, err)
			}

			for _, key := range generationBoundMetadataKeys {
				output, err := run("show-environment", "-t", tmuxSession, key)
				if key != tt.seedKey {
					if err == nil {
						t.Fatalf("generation group filled %s=%q despite existing %s", key, strings.TrimSpace(output), tt.seedKey)
					}
					continue
				}
				want := key + "=" + tt.seedValue
				if tt.removed {
					want = "-" + key
				}
				if err != nil || strings.TrimSpace(output) != want {
					t.Fatalf("session %s = %q, %v; want %q", key, strings.TrimSpace(output), err, want)
				}
			}
		})
	}
}

func TestK8sLegacyRelaunchIdentityReadPreservesNewGenerationWhenSessionIDMatchesGlobal(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	oldEnv := map[string]string{
		"GC_SESSION_ID":         "stable-session",
		"GC_INSTANCE_TOKEN":     "pod-instance",
		"GC_RUNTIME_EPOCH":      "pod-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "pod-continuation-epoch",
		"GC_PROVIDER":           "claude",
	}
	for key, value := range oldEnv {
		t.Setenv(key, value)
	}
	if _, err := run("set-environment", "-g", "GC_SESSION_ID", "stable-session"); err != nil {
		t.Fatalf("set matching global session ID: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create warm tmux session: %v", err)
	}

	p := newProviderWithOps(ops)
	p.cityPath = city
	p.postStartSettle = 0
	if err := p.seedStartMetadata(context.Background(), "worker", "worker", oldEnv); err != nil {
		t.Fatalf("seed pod-creation identity: %v", err)
	}
	newEnv := map[string]string{
		"GC_SESSION_ID":         "stable-session",
		"GC_INSTANCE_TOKEN":     "relaunched-instance",
		"GC_RUNTIME_EPOCH":      "relaunched-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "relaunched-continuation-epoch",
		"GC_PROVIDER":           "codex",
	}
	if err := p.Relaunch(context.Background(), "worker", runtime.Config{Command: "sleep 300", Env: newEnv}); err != nil {
		t.Fatalf("warm Relaunch: %v", err)
	}
	if _, err := run("set-environment", "-h", "-t", tmuxSession, "-r", identityMetadataVersionKey); err != nil {
		t.Fatalf("remove identity version marker to model legacy relaunch: %v", err)
	}
	if current, err := p.hasCurrentIdentityMetadataVersion(context.Background(), "worker"); err != nil || current {
		t.Fatalf("legacy relaunch marker = %t, %v; want marker absent", current, err)
	}

	for key, want := range map[string]string{
		"GC_SESSION_ID":         "stable-session",
		"GC_INSTANCE_TOKEN":     "relaunched-instance",
		"BEADS_HOLDER_TOKEN":    "relaunched-instance",
		"GC_RUNTIME_EPOCH":      "relaunched-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "relaunched-continuation-epoch",
		"GC_PROVIDER":           "codex",
	} {
		got, err := p.GetMeta("worker", key)
		if err != nil || got != want {
			t.Fatalf("legacy relaunch identity GetMeta(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	if current, err := p.hasCurrentIdentityMetadataVersion(context.Background(), "worker"); err != nil || current {
		t.Fatalf("legacy relaunch marker after read = %t, %v; want marker absent", current, err)
	}
}

func TestK8sRuntimeOwnerBackfillPreservesExistingAndFillsMissingValues(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	if _, err := run("set-environment", "-g", "GC_CITY_PATH", "/session/city"); err != nil {
		t.Fatalf("set global runtime-owner path: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create legacy tmux session: %v", err)
	}
	if _, err := run("set-environment", "-t", tmuxSession, "GC_CITY_PATH", "/session/city"); err != nil {
		t.Fatalf("set existing session runtime-owner path: %v", err)
	}
	t.Setenv("GC_CITY_PATH", "/pod-creation/city")
	t.Setenv("GT_PROCESS_NAMES", "claude,codex")
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()

	if got, err := p.GetMeta("worker", "GC_CITY_PATH"); err != nil || got != "/session/city" {
		t.Fatalf("runtime-owner path after backfill = %q, %v; want existing session value", got, err)
	}
	if got, err := p.GetMeta("worker", "GT_PROCESS_NAMES"); err != nil || got != "claude,codex" {
		t.Fatalf("missing runtime-owner backfill = %q, %v; want pod value", got, err)
	}
	if got, err := run("show-environment", "-t", tmuxSession, "GC_CITY_PATH"); err != nil || strings.TrimSpace(got) != "GC_CITY_PATH=/session/city" {
		t.Fatalf("session runtime-owner path after backfill = %q, %v; want preserved value", got, err)
	}
}

func TestK8sLegacyBackfillDoesNotMixAdoptedGeneration(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create adopted tmux session: %v", err)
	}
	for key, value := range map[string]string{
		"GC_INSTANCE_TOKEN": "adoption-minted-instance",
		"GC_RUNTIME_EPOCH":  "adoption-runtime-epoch",
		"GC_SESSION_ID":     "adoption-session",
	} {
		if _, err := run("set-environment", "-t", tmuxSession, key, value); err != nil {
			t.Fatalf("seed adoption identity %s: %v", key, err)
		}
	}
	for key, value := range map[string]string{
		"GC_INSTANCE_TOKEN":     "pod-instance-token",
		"GC_RUNTIME_EPOCH":      "pod-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "pod-continuation-epoch",
		"GC_SESSION_ID":         "pod-session",
		"GC_PROVIDER":           "pod-provider",
	} {
		t.Setenv(key, value)
	}
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	if got, err := p.GetMeta("worker", "GC_PROVIDER"); err != nil || got != "pod-provider" {
		t.Fatalf("descriptive identity backfill = %q, %v", got, err)
	}
	for key, want := range map[string]string{
		"GC_INSTANCE_TOKEN": "adoption-minted-instance",
		"GC_RUNTIME_EPOCH":  "adoption-runtime-epoch",
		"GC_SESSION_ID":     "adoption-session",
	} {
		got, err := run("show-environment", "-t", tmuxSession, key)
		if err != nil || strings.TrimSpace(got) != key+"="+want {
			t.Fatalf("adoption-stamped %s = %q, %v; want %q", key, got, err, want)
		}
	}
	for _, key := range []string{"BEADS_HOLDER_TOKEN", "GC_CONTINUATION_EPOCH"} {
		if _, err := run("show-environment", "-t", tmuxSession, key); err == nil {
			t.Fatalf("legacy pod value filled adoption-generation key %s", key)
		}
	}
	if got, err := p.GetMeta("worker", "BEADS_HOLDER_TOKEN"); err != nil || got != "" {
		t.Fatalf("adoption holder token = %q, %v; want no mismatched pod token", got, err)
	}
}

func TestK8sLegacyBackfillNestedTmuxQuotingViaTMUX(t *testing.T) {
	ops, run := newTmuxMetadataTestOpsWithTMUX(t, "worker")
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create legacy tmux session: %v", err)
	}
	specialInstance := `instance 'quoted' "$HOME" \\ #{pane_current_path}; second-command`
	specialSession := `session 'quoted' "$HOME" \\ #{pane_current_path}; second-command`
	t.Setenv("GC_INSTANCE_TOKEN", specialInstance)
	t.Setenv("GC_SESSION_ID", specialSession)
	t.Setenv("GC_RUNTIME_EPOCH", "runtime-epoch")
	t.Setenv("GC_CONTINUATION_EPOCH", "continuation-epoch")
	t.Setenv("GC_PROVIDER", "codex")
	p := newProviderWithOps(ops)
	p.cityPath = t.TempDir()
	if got, err := p.GetMeta("worker", "GC_SESSION_ID"); err != nil || got != specialSession {
		t.Fatalf("nested-quoted session identity = %q, %v; want exact value", got, err)
	}
	for key, want := range map[string]string{
		"GC_INSTANCE_TOKEN":  specialInstance,
		"BEADS_HOLDER_TOKEN": specialInstance,
		"GC_SESSION_ID":      specialSession,
	} {
		got, err := run("show-environment", "-t", tmuxSession, key)
		if err != nil || strings.TrimSuffix(got, "\n") != key+"="+want {
			t.Fatalf("tmux value %s = %q, %v; want exact nested-if-shell value", key, got, err)
		}
	}
	if ops.tmuxEnv == "" {
		t.Fatal("test fixture did not exercise tmux discovery through $TMUX")
	}
}

func TestK8sRelaunchIdentityReadPreservesNewGenerationWhenSessionIDMatchesGlobal(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	city := t.TempDir()
	t.Setenv("GC_CITY_PATH", city)
	oldEnv := map[string]string{
		"GC_SESSION_ID":         "stable-session",
		"GC_INSTANCE_TOKEN":     "pod-instance",
		"GC_RUNTIME_EPOCH":      "pod-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "pod-continuation-epoch",
		"GC_PROVIDER":           "claude",
	}
	for key, value := range oldEnv {
		t.Setenv(key, value)
	}
	if _, err := run("set-environment", "-g", "GC_SESSION_ID", "stable-session"); err != nil {
		t.Fatalf("set matching global session ID: %v", err)
	}
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create warm tmux session: %v", err)
	}

	p := newProviderWithOps(ops)
	p.cityPath = city
	p.postStartSettle = 0
	if err := p.seedStartMetadata(context.Background(), "worker", "worker", oldEnv); err != nil {
		t.Fatalf("seed pod-creation identity: %v", err)
	}
	newEnv := map[string]string{
		"GC_SESSION_ID":         "stable-session",
		"GC_INSTANCE_TOKEN":     "relaunched-instance",
		"GC_RUNTIME_EPOCH":      "relaunched-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "relaunched-continuation-epoch",
		"GC_PROVIDER":           "codex",
	}
	if err := p.Relaunch(context.Background(), "worker", runtime.Config{Command: "sleep 300", Env: newEnv}); err != nil {
		t.Fatalf("warm Relaunch: %v", err)
	}

	if got, err := p.GetMeta("worker", "GC_SESSION_ID"); err != nil || got != "stable-session" {
		t.Fatalf("identity after relaunch GetMeta(GC_SESSION_ID) = %q, %v; want stable-session", got, err)
	}
	for key, want := range map[string]string{
		"GC_INSTANCE_TOKEN":     "relaunched-instance",
		"BEADS_HOLDER_TOKEN":    "relaunched-instance",
		"GC_RUNTIME_EPOCH":      "relaunched-runtime-epoch",
		"GC_CONTINUATION_EPOCH": "relaunched-continuation-epoch",
		"GC_PROVIDER":           "codex",
	} {
		got, err := p.GetMeta("worker", key)
		if err != nil || got != want {
			t.Fatalf("identity after relaunch GetMeta(%s) = %q, %v; want %q", key, got, err, want)
		}
	}
	marker, err := run("show-environment", "-h", "-t", tmuxSession, identityMetadataVersionKey)
	if err != nil || strings.TrimSpace(marker) != identityMetadataVersionKey+"="+identityMetadataVersion {
		t.Fatalf("current identity marker = %q, %v", marker, err)
	}
	for _, call := range ops.calls {
		if strings.Contains(strings.Join(call.cmd, " "), `printenv "$1"`) {
			t.Fatal("current relaunch identity triggered legacy container-environment backfill")
		}
	}
}

func TestK8sRelaunchFromTrulyLegacySessionWithoutIdentityFields(t *testing.T) {
	ops, run := newTmuxMetadataTestOps(t, "worker")
	city := t.TempDir()
	if _, err := run("new-session", "-d", "-s", tmuxSession, "sleep", "300"); err != nil {
		t.Fatalf("create metadata-free legacy session: %v", err)
	}
	for _, key := range append(append([]string(nil), orderedSessionIdentityKeys...), runtimeOwnerEnvKeys...) {
		if output, err := run("show-environment", "-t", tmuxSession, key); err == nil {
			t.Fatalf("legacy session unexpectedly has %s=%q", key, strings.TrimSpace(output))
		}
	}
	p := newProviderWithOps(ops)
	p.cityPath = city
	p.postStartSettle = 0
	env := map[string]string{
		"GC_SESSION_ID":         "legacy-relaunch-session",
		"GC_INSTANCE_TOKEN":     "legacy-relaunch-instance",
		"GC_RUNTIME_EPOCH":      "legacy-relaunch-runtime",
		"GC_CONTINUATION_EPOCH": "legacy-relaunch-continuation",
		"GC_PROVIDER":           "codex",
	}
	if err := p.Relaunch(context.Background(), "worker", runtime.Config{Command: "sleep 300", Env: env}); err != nil {
		t.Fatalf("Relaunch from metadata-free legacy session: %v", err)
	}
	for key, want := range map[string]string{
		"GC_SESSION_ID":         "legacy-relaunch-session",
		"GC_INSTANCE_TOKEN":     "legacy-relaunch-instance",
		"BEADS_HOLDER_TOKEN":    "legacy-relaunch-instance",
		"GC_RUNTIME_EPOCH":      "legacy-relaunch-runtime",
		"GC_CONTINUATION_EPOCH": "legacy-relaunch-continuation",
		"GC_PROVIDER":           "codex",
	} {
		got, err := p.GetMeta("worker", key)
		if err != nil || got != want {
			t.Fatalf("legacy Relaunch metadata %s = %q, %v; want %q", key, got, err, want)
		}
	}
	if current, err := p.hasCurrentIdentityMetadataVersion(context.Background(), "worker"); err != nil || !current {
		t.Fatalf("legacy Relaunch identity version marker = %t, %v; want current marker", current, err)
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
