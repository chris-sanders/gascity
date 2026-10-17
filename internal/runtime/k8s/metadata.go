package k8s

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	k8sMetaReadMissing = 44
	k8sMetaOpTimeout   = 15 * time.Second
	identityUnboundKey = "__gc_identity_unbound"
)

var metadataKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// K8s metadata has separate identity, shared protocol, runtime-owner, and
// controller-private classes. Only the explicit identity/runtime-owner
// allowlist is copied from launch configuration; arbitrary environment values
// never enter tmux metadata.
var sessionIdentityEnv = map[string]bool{
	"GC_SESSION_ID":         true,
	"GC_SESSION_NAME":       true,
	"GC_RUNTIME_EPOCH":      true,
	"GC_CONTINUATION_EPOCH": true,
	"GC_INSTANCE_TOKEN":     true,
	"BEADS_HOLDER_TOKEN":    true,
	"GC_ALIAS":              true,
	"GC_AGENT":              true,
	"BEADS_ACTOR":           true,
	"GC_TEMPLATE":           true,
	"GC_SESSION_ORIGIN":     true,
	"GC_PROVIDER":           true,
}

var orderedSessionIdentityKeys = []string{
	"GC_INSTANCE_TOKEN",
	"BEADS_HOLDER_TOKEN",
	"GC_RUNTIME_EPOCH",
	"GC_CONTINUATION_EPOCH",
	"GC_SESSION_NAME",
	"GC_ALIAS",
	"GC_AGENT",
	"BEADS_ACTOR",
	"GC_TEMPLATE",
	"GC_SESSION_ORIGIN",
	"GC_PROVIDER",
	"GC_SESSION_ID",
}

var protocolMetadataKeys = []string{
	"GC_DRAIN",
	"GC_DRAIN_ACK",
	"GC_DRAIN_ACK_SOURCE",
	"GC_DRAIN_REASON",
	"GC_DRAIN_GENERATION",
	"GC_DRAIN_ACK_REQUESTER_INSTANCE_TOKEN",
	"GC_RESTART_REQUESTED",
	"GC_DRIFT_RESTART",
}

var runtimeOwnerEnvKeys = []string{
	"GC_CITY_PATH",
	"GT_PROCESS_NAMES",
}

type metadataClass uint8

const (
	metadataControllerPrivate metadataClass = iota
	metadataIdentity
	metadataProtocol
	metadataRuntimeOwner
)

func classifyMetadataKey(key string) metadataClass {
	if sessionIdentityEnv[key] {
		return metadataIdentity
	}
	for _, candidate := range protocolMetadataKeys {
		if key == candidate {
			return metadataProtocol
		}
	}
	for _, candidate := range runtimeOwnerEnvKeys {
		if key == candidate {
			return metadataRuntimeOwner
		}
	}
	return metadataControllerPrivate
}

func isTmuxMetadataKey(key string) bool {
	class := classifyMetadataKey(key)
	return class == metadataIdentity || class == metadataProtocol || class == metadataRuntimeOwner
}

func isBackfillableMetadataKey(key string) bool {
	return classifyMetadataKey(key) == metadataIdentity || classifyMetadataKey(key) == metadataRuntimeOwner
}

// buildRespawnCommandAsTmuxOwner is used when the exec connection itself has
// already switched to LINUX_USERNAME. It preserves the legacy working
// directory while avoiding a second su and reaching the same tmux socket.
func buildRespawnCommandAsTmuxOwner(cfg runtime.Config) []string {
	ownerCfg := cfg
	ownerCfg.Env = maps.Clone(cfg.Env)
	delete(ownerCfg.Env, "LINUX_USERNAME")
	script := buildAgentLaunchCommand(ownerCfg, fmt.Sprintf("tmux respawn-pane -k -t %s", tmuxSession), false)
	if cfg.Env["LINUX_USERNAME"] != "" {
		script = "cd " + shellquote.Quote(projectedPodWorkDir(cfg)) + " && " + script
	}
	return append([]string{"sh", "-c"}, agentCommandShellArgs(script, ownerCfg)...)
}

// buildRespawnCommandWithIdentityFile wraps the pane's command so it receives
// the staged identity from a private file before the agent starts. The tmux
// session environment is published only after the replacement pane binds.
func buildRespawnCommandWithIdentityFile(cfg runtime.Config, identityPath, identityBoundPath string) []string {
	wrapped := cfg
	wrapped.Env = maps.Clone(cfg.Env)
	parts := make([]string, 0, len(orderedSessionIdentityKeys)+len(runtimeOwnerEnvKeys)+5)
	for _, key := range append(append([]string(nil), orderedSessionIdentityKeys...), runtimeOwnerEnvKeys...) {
		parts = append(parts, "unset "+key)
	}
	quotedPath := shellquote.Quote(identityPath)
	parts = append(parts,
		"set -a",
		". "+quotedPath,
		"set +a",
		"rm -f -- "+quotedPath,
		": > "+shellquote.Quote(identityBoundPath),
		"exec sh -c "+shellquote.Quote(cfg.Command),
	)
	wrapped.Command = strings.Join(parts, "; ")
	if cfg.Env["LINUX_USERNAME"] != "" {
		return buildRespawnCommandAsTmuxOwner(wrapped)
	}
	return buildRespawnCommand(wrapped)
}

// seedStartMetadata publishes the explicit identity and runtime-owner
// allowlists, derives BEADS_HOLDER_TOKEN from GC_INSTANCE_TOKEN, and clears all
// shared protocol values for the new generation. Values travel only over exec
// stdin into a private command file.
func (p *Provider) seedStartMetadata(ctx context.Context, podName, sessionName string, env map[string]string) error {
	if err := p.publishSessionMetadata(ctx, podName, sessionName, env, true, true, true); err != nil {
		return err
	}
	return nil
}

func (p *Provider) publishSessionMetadata(ctx context.Context, podName, sessionName string, env map[string]string, publishIdentity, publishRuntimeOwner, clearProtocol bool) error {
	commands := sessionMetadataCommands(env, publishIdentity, publishRuntimeOwner, clearProtocol)
	if commands == "" {
		return nil
	}
	if err := p.sourcePrivateTmuxFile(ctx, podName, commands); err != nil {
		return fmt.Errorf("k8s runtime metadata publication failed for session %q", sessionName)
	}
	return nil
}

func sessionMetadataCommands(env map[string]string, publishIdentity, publishRuntimeOwner, clearProtocol bool) string {
	lines := make([]string, 0, len(orderedSessionIdentityKeys)*2+len(runtimeOwnerEnvKeys)+len(protocolMetadataKeys))
	if publishIdentity {
		for _, key := range orderedSessionIdentityKeys {
			lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", key))
		}
	}
	if publishRuntimeOwner {
		for _, key := range runtimeOwnerEnvKeys {
			lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", key))
		}
	}
	if clearProtocol {
		for _, key := range protocolMetadataKeys {
			lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "-r", key))
		}
	}
	if publishIdentity {
		for _, key := range orderedSessionIdentityKeys {
			if key == "GC_SESSION_ID" {
				continue
			}
			value, ok := env[key]
			if key == "BEADS_HOLDER_TOKEN" {
				value, ok = env["GC_INSTANCE_TOKEN"]
			}
			if !ok || value == "" {
				continue
			}
			lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, key, value))
		}
	}
	if publishRuntimeOwner {
		for _, key := range runtimeOwnerEnvKeys {
			if value := env[key]; value != "" {
				lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, key, value))
			}
		}
	}
	if publishIdentity {
		if sessionID := env["GC_SESSION_ID"]; sessionID != "" {
			lines = append(lines, tmuxMetadataCommand("set-environment", "-t", tmuxSession, "GC_SESSION_ID", sessionID))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// respawnWithStagedIdentity stages identity in a private environment file for
// the new pane, waits for that pane to source the file and remain live, then
// publishes the tmux session identity. The controller-side tombstone stays in
// place through the subsequent post-launch liveness check.
func (p *Provider) respawnWithStagedIdentity(ctx context.Context, podName, sessionName string, env map[string]string, cfg runtime.Config) error {
	identityFile := sessionMetadataEnvironmentFile(env)
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("k8s relaunch %q: identity staging nonce unavailable", sessionName)
	}
	dir := "/tmp/gc-tmux-meta-" + hex.EncodeToString(nonce)
	identityPath := filepath.Join(dir, "identity.env")
	identityBoundPath := filepath.Join(dir, "identity.bound")
	respawnCommand := buildRespawnCommandWithIdentityFile(cfg, identityPath, identityBoundPath)
	const script = `set +x
set -eu
umask 077
lockdir=/tmp/gc-controller-locks
mkdir -p "$lockdir"
chmod 700 "$lockdir"
exec 9>"$lockdir/tmux-metadata.lock"
flock -x -w 10 9
dir=$1
bound=$2
shift 2
mkdir "$dir"
chmod 700 "$dir"
file="$dir/identity.env"
cleanup() { rm -rf "$dir"; }
trap cleanup EXIT HUP INT TERM
( set -C; cat > "$file" )
chmod 600 "$file"
old_pid=$(tmux display-message -p -t main '#{pane_pid}')
if "$@"; then :; else
  code=$?
  exit "$code"
fi
attempt=0
while [ "$attempt" -lt 100 ]; do
  attempt=$((attempt + 1))
  state=$(tmux display-message -p -t main '#{pane_pid} #{pane_dead}') || state=''
  set -- $state
  new_pid=${1:-}
  pane_dead=${2:-1}
	if [ -f "$bound" ] && [ -n "$new_pid" ] && [ "$new_pid" != "$old_pid" ] && [ "$pane_dead" = 0 ] && kill -0 "$new_pid" 2>/dev/null; then
    exit 0
  fi
  sleep 0.1
done
exit 75`
	argv := append([]string{"sh", "-c", script, "sh", dir, identityBoundPath}, respawnCommand...)
	_, err := p.execTmuxOwner(ctx, podName, "agent", argv, strings.NewReader(identityFile))
	if err != nil {
		return fmt.Errorf("k8s relaunch %q: replacement pane did not bind", sessionName)
	}
	if err := p.publishSessionMetadata(ctx, podName, sessionName, env, true, true, false); err != nil {
		return fmt.Errorf("k8s relaunch %q: replacement pane bound but identity publication failed", sessionName)
	}
	return nil
}

func sessionMetadataEnvironmentFile(env map[string]string) string {
	keys := append(append([]string(nil), orderedSessionIdentityKeys...), runtimeOwnerEnvKeys...)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := env[key]
		if key == "BEADS_HOLDER_TOKEN" {
			value, ok = env["GC_INSTANCE_TOKEN"]
		}
		if ok && value != "" {
			lines = append(lines, key+"="+shellquote.Quote(value))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// sourcePrivateTmuxFile streams tmux commands over exec stdin into an exclusive
// 0600 file under a 0700 directory, sources it while holding the pod's
// cross-process metadata lock, then removes the directory. Command values never
// enter the process argv or an error string.
func (p *Provider) sourcePrivateTmuxFile(ctx context.Context, podName, commandFile string) error {
	const script = `set +x
set -eu
umask 077
lockdir=/tmp/gc-controller-locks
mkdir -p "$lockdir"
chmod 700 "$lockdir"
exec 9>"$lockdir/tmux-metadata.lock"
flock -x -w 10 9
dir=$(mktemp -d /tmp/gc-tmux-meta.XXXXXX)
chmod 700 "$dir"
file="$dir/commands.tmux"
cleanup() { rm -rf "$dir"; }
trap cleanup EXIT HUP INT TERM
( set -C; cat > "$file" )
chmod 600 "$file"
tmux source-file "$file"`
	_, err := p.execTmuxOwner(ctx, podName, "agent", []string{"sh", "-c", script}, strings.NewReader(commandFile))
	if err != nil {
		return fmt.Errorf("private tmux metadata command failed")
	}
	return nil
}

func (p *Provider) execTmuxMetadataRead(ctx context.Context, podName string, args ...string) (string, error) {
	const script = `set +x
set -eu
umask 077
lockdir=/tmp/gc-controller-locks
mkdir -p "$lockdir"
chmod 700 "$lockdir"
exec 9>"$lockdir/tmux-metadata.lock"
flock -s -w 10 9
exec tmux "$@"`
	cmd := append([]string{"sh", "-c", script, "sh"}, args...)
	return p.execTmuxOwner(ctx, podName, "agent", cmd, nil)
}

func (p *Provider) setPrivateMetadata(ctx context.Context, sessionName, key, value string) error {
	if !metadataKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid metadata key")
	}
	return p.withControllerMetadataLock(ctx, sessionName, func(root string) error {
		tmp, err := os.CreateTemp(root, ".value-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name()) //nolint:errcheck
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close() //nolint:errcheck
			return err
		}
		if _, err := tmp.WriteString(value); err != nil {
			tmp.Close() //nolint:errcheck
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close() //nolint:errcheck
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), controllerMetadataValuePath(root, sessionName, key)); err != nil {
			return err
		}
		return syncControllerMetadataDir(root)
	})
}

func (p *Provider) getPrivateMetadata(ctx context.Context, name, key string) (string, error) {
	if !metadataKeyPattern.MatchString(key) {
		return "", fmt.Errorf("invalid metadata key")
	}
	var value string
	err := p.withControllerMetadataLock(ctx, name, func(root string) error {
		contents, err := os.ReadFile(controllerMetadataValuePath(root, name, key))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		value = string(contents)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("controller-private metadata read failed for session %q", name)
	}
	return value, nil
}

func (p *Provider) removePrivateMetadata(ctx context.Context, name, key string) error {
	if !metadataKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid metadata key")
	}
	return p.withControllerMetadataLock(ctx, name, func(root string) error {
		if err := os.Remove(controllerMetadataValuePath(root, name, key)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncControllerMetadataDir(root)
	})
}

// Controller-private metadata belongs beside the controller's city state. A
// same-UID file in the agent pod is visible to the agent and is only suitable
// as command transport or a checkpoint, never as the source of truth.
func (p *Provider) withControllerMetadataLock(ctx context.Context, sessionName string, fn func(root string) error) error {
	root, err := controllerMetadataRoot(p.cityPath)
	if err != nil {
		return err
	}
	lockName := controllerMetadataSessionKey(sessionName)
	lock, err := os.OpenFile(filepath.Join(root, ".lock-"+lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck
	if err := lock.Chmod(0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(k8sMetaOpTimeout)
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn(root)
}

func controllerMetadataRoot(city string) (string, error) {
	city = strings.TrimSpace(city)
	if city == "" {
		city = processCityPath()
	}
	if city == "" {
		configDir, err := os.UserConfigDir()
		if err != nil || strings.TrimSpace(configDir) == "" {
			return "", errors.New("controller-owned metadata directory is unavailable")
		}
		city = filepath.Join(configDir, "gascity", "default-city")
	}
	root := filepath.Join(city, ".gc", "runtime-meta", "k8s")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

func (p *Provider) acquireControllerGenerationLock(ctx context.Context, sessionName string) (func(), error) {
	root, err := controllerMetadataRoot(p.cityPath)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".generation-"+controllerMetadataSessionKey(sessionName)), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close() //nolint:errcheck
		return nil, err
	}
	deadline := time.Now().Add(k8sMetaOpTimeout)
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				_ = lock.Close()
			}, nil
		} else if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			lock.Close() //nolint:errcheck
			return nil, err
		}
		if ctx.Err() != nil {
			lock.Close() //nolint:errcheck
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			lock.Close() //nolint:errcheck
			return nil, context.DeadlineExceeded
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.Close() //nolint:errcheck
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func controllerMetadataSessionKey(sessionName string) string {
	sum := sha256.Sum256([]byte(sessionName))
	return hex.EncodeToString(sum[:])
}

func controllerMetadataValuePath(root, sessionName, key string) string {
	return filepath.Join(root, controllerMetadataSessionKey(sessionName)+"-"+key)
}

func syncControllerMetadataDir(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck
	return dir.Sync()
}

// getMetaFromPod reads an allowlisted shared value from tmux. Existing pods
// predating the metadata allowlist are repaired from their container's own
// environment when the session key is absent or still inherited from tmux's
// global environment.
func (p *Provider) getMetaFromPod(ctx context.Context, podName, name, key string) (string, error) {
	if !metadataKeyPattern.MatchString(key) || !isTmuxMetadataKey(key) {
		return "", fmt.Errorf("invalid shared metadata key")
	}
	if classifyMetadataKey(key) == metadataIdentity && key != identityUnboundKey {
		unbound, err := p.getPrivateMetadata(ctx, name, identityUnboundKey)
		if err != nil {
			return "", fmt.Errorf("k8s verify identity binding for session %q failed", name)
		}
		if unbound == "1" {
			return "", nil
		}
	}
	read := func() (string, error) {
		return p.execTmuxMetadataRead(ctx, podName, "show-environment", "-t", tmuxSession, key)
	}
	output, err := read()
	if err != nil {
		if !isExecExitStatus(err, 1) {
			return "", fmt.Errorf("k8s read metadata key %q for session %q: %w", key, name, err)
		}
		if _, sessionErr := p.execTmuxMetadataRead(ctx, podName, "has-session", "-t", tmuxSession); sessionErr != nil {
			return "", fmt.Errorf("k8s verify tmux session while reading metadata key %q for %q: %w", key, name, sessionErr)
		}
		if !isBackfillableMetadataKey(key) {
			return "", nil
		}
		if err := p.backfillMetadataFromPodEnv(ctx, podName, name, key); err != nil {
			return "", err
		}
		output, err = read()
		if err != nil {
			if isExecExitStatus(err, 1) {
				return "", nil
			}
			return "", fmt.Errorf("k8s read backfilled metadata key %q for session %q: %w", key, name, err)
		}
	}
	value, explicitRemoval, err := parseTmuxMetadataOutput(output, key)
	if err != nil {
		return "", fmt.Errorf("k8s read metadata key %q for session %q: %w", key, name, err)
	}
	if explicitRemoval || !isBackfillableMetadataKey(key) {
		return value, nil
	}
	global, globalErr := p.execTmuxMetadataRead(ctx, podName, "show-environment", "-g", key)
	if globalErr == nil {
		globalValue, removed, parseErr := parseTmuxMetadataOutput(global, key)
		if parseErr == nil && !removed && globalValue == value {
			if err := p.backfillMetadataFromPodEnv(ctx, podName, name, key); err != nil {
				return "", err
			}
			output, err = read()
			if err != nil {
				return "", fmt.Errorf("k8s read repaired metadata key %q for session %q: %w", key, name, err)
			}
			value, _, err = parseTmuxMetadataOutput(output, key)
			if err != nil {
				return "", fmt.Errorf("k8s read repaired metadata key %q for session %q: %w", key, name, err)
			}
		}
	}
	return value, nil
}

func (p *Provider) backfillMetadataFromPodEnv(ctx context.Context, podName, name, requestedKey string) error {
	class := classifyMetadataKey(requestedKey)
	keys := runtimeOwnerEnvKeys
	publishIdentity, publishRuntimeOwner := false, false
	if class == metadataIdentity {
		keys = orderedSessionIdentityKeys
		publishIdentity = true
	} else {
		publishRuntimeOwner = true
	}
	env := make(map[string]string, len(keys))
	for _, key := range keys {
		if key == "BEADS_HOLDER_TOKEN" {
			continue
		}
		value, err := p.readPodEnvValue(ctx, podName, key)
		if err != nil {
			return fmt.Errorf("k8s compatibility backfill for session %q failed", name)
		}
		env[key] = value
	}
	if err := p.publishSessionMetadata(ctx, podName, name, env, publishIdentity, publishRuntimeOwner, false); err != nil {
		return fmt.Errorf("k8s compatibility backfill for session %q failed", name)
	}
	return nil
}

func (p *Provider) readPodEnvValue(ctx context.Context, podName, key string) (string, error) {
	const script = `set +x
set -eu
value=$(printenv "$1" 2>/dev/null) || exit 44
printf '%s' "$value"`
	value, err := p.execTmuxOwner(ctx, podName, "agent", []string{"sh", "-c", script, "sh", key}, nil)
	if err != nil && isExecExitStatus(err, k8sMetaReadMissing) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading allowlisted pod environment failed")
	}
	return strings.TrimSuffix(value, "\n"), nil
}

func sessionMetadataEnvForConfig(cfg runtime.Config) map[string]string {
	env := cfg.Env
	needsClone := strings.TrimSpace(env["GC_PROVIDER"]) == "" && strings.TrimSpace(cfg.ProviderName) != ""
	ctrlCity := controllerCityPath(env)
	projectedCityPath := remapControllerPathToPod(env["GC_CITY_PATH"], ctrlCity)
	needsClone = needsClone || projectedCityPath != env["GC_CITY_PATH"]
	if !needsClone {
		return env
	}
	clone := make(map[string]string, len(env)+1)
	for key, value := range env {
		clone[key] = value
	}
	if strings.TrimSpace(clone["GC_PROVIDER"]) == "" {
		clone["GC_PROVIDER"] = strings.TrimSpace(cfg.ProviderName)
	}
	if projectedCityPath != clone["GC_CITY_PATH"] {
		clone["GC_CITY_PATH"] = projectedCityPath
	}
	return clone
}

func parseTmuxMetadataOutput(output, key string) (value string, explicitlyRemoved bool, err error) {
	output = strings.TrimSuffix(output, "\n")
	output = strings.TrimSuffix(output, "\r")
	if output == "-"+key {
		return "", true, nil
	}
	if gotKey, gotValue, ok := strings.Cut(output, "="); ok && gotKey == key {
		return gotValue, false, nil
	}
	if output == "" {
		return "", false, nil
	}
	return "", false, errors.New("unexpected tmux response")
}

func isExecExitStatus(err error, status int) bool {
	var exitErr execerr.ExitError
	return errors.As(err, &exitErr) && exitErr.Exited() && exitErr.ExitStatus() == status
}

// tmuxMetadataCommand quotes each config argument as one tmux literal. Command
// values are parsed from a private source-file rather than process argv.
func tmuxMetadataCommand(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
