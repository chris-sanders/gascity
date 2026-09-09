package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Compile-time interface checks.
var (
	_ runtime.Provider             = (*Provider)(nil)
	_ runtime.ExecProvider         = (*Provider)(nil)
	_ runtime.InstanceTokenStopper = (*Provider)(nil)
)

// Provider is a native Kubernetes session provider using client-go.
// Eliminates subprocess overhead by making direct API calls over reused
// HTTP/2 connections. Pod manifests are compatible with gc-session-k8s.
type Provider struct {
	ops                  k8sOps
	namespace            string
	image                string
	k8sContext           string
	managedServiceHost   string
	managedServicePort   string
	cpuRequest           string
	memRequest           string
	cpuLimit             string
	memLimit             string
	serviceAccount       string              // pod service account name (GC_K8S_SERVICE_ACCOUNT)
	prebaked             bool                // skip staging + init container for prebaked images
	nodeSelector         map[string]string   // GC_K8S_NODE_SELECTOR (JSON)
	tolerations          []corev1.Toleration // GC_K8S_TOLERATIONS (JSON)
	affinity             *corev1.Affinity    // GC_K8S_AFFINITY (JSON)
	priorityClassName    string              // GC_K8S_PRIORITY_CLASS_NAME
	codexAuthSecret      string              // GC_K8S_CODEX_AUTH_SECRET: read-only auth.json seed
	postStartSettle      time.Duration       // settle time before post-start liveness check
	startupDialogTimeout time.Duration       // shared startup-dialog budget; zero uses runtime default
	startupReadyTimeout  time.Duration       // bounded wait for a configured interactive prompt
	stderr               io.Writer           // warning output (default os.Stderr)
	providerMu           sync.RWMutex
	providerNames        map[string]string // session name → resolved provider name
}

type schedulingFields struct {
	nodeSelector      map[string]string
	tolerations       []corev1.Toleration
	affinity          *corev1.Affinity
	priorityClassName string
}

// NewProvider creates a K8s session provider.
// Configuration is read from environment variables (matching gc-session-k8s):
//   - GC_K8S_NAMESPACE — namespace (default: "gc")
//   - GC_K8S_IMAGE — container image (required for Start)
//   - GC_K8S_CONTEXT — kubectl context (default: current)
//   - GC_K8S_SERVICE_ACCOUNT — pod service account name (default: namespace default)
//   - GC_K8S_CPU_REQUEST, GC_K8S_MEM_REQUEST — resource requests
//   - GC_K8S_CPU_LIMIT, GC_K8S_MEM_LIMIT — resource limits
//   - GC_K8S_CODEX_AUTH_SECRET — optional Secret containing a minimal Codex
//     auth.json seed.  It is mounted read-only and copied into a fresh writable
//     CODEX_HOME for each worker; it is never injected as an API-key variable.
//
// The in-cluster Dolt service alias defaults to the provider defaults
// (dolt.gc.svc.cluster.local:3307). Pods receive projected GC_DOLT_* env;
// GC_K8S_DOLT_* remains a deprecated compatibility input for the provider-
// managed in-cluster alias only.
//
// Uses rest.InClusterConfig() when running in a pod, falls back to
// clientcmd.BuildConfigFromFlags() for local development.
func NewProvider() (*Provider, error) {
	namespace := envOrDefault("GC_K8S_NAMESPACE", "gc")
	image := os.Getenv("GC_K8S_IMAGE")
	k8sContext := os.Getenv("GC_K8S_CONTEXT")

	restConfig, err := buildRESTConfig(k8sContext)
	if err != nil {
		return nil, fmt.Errorf("building K8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating K8s clientset: %w", err)
	}

	managedServiceHost, managedServicePort, err := managedServiceAlias()
	if err != nil {
		return nil, err
	}

	scheduling, err := parseSchedulingEnv()
	if err != nil {
		return nil, err
	}

	return &Provider{
		ops: &realK8sOps{
			clientset:  clientset,
			restConfig: restConfig,
			namespace:  namespace,
		},
		namespace:            namespace,
		image:                image,
		k8sContext:           k8sContext,
		managedServiceHost:   managedServiceHost,
		managedServicePort:   managedServicePort,
		cpuRequest:           envOrDefault("GC_K8S_CPU_REQUEST", "500m"),
		memRequest:           envOrDefault("GC_K8S_MEM_REQUEST", "1Gi"),
		cpuLimit:             envOrDefault("GC_K8S_CPU_LIMIT", "2"),
		memLimit:             envOrDefault("GC_K8S_MEM_LIMIT", "4Gi"),
		serviceAccount:       os.Getenv("GC_K8S_SERVICE_ACCOUNT"),
		prebaked:             os.Getenv("GC_K8S_PREBAKED") == "true",
		postStartSettle:      3 * time.Second,
		startupDialogTimeout: runtime.StartupDialogTimeout(),
		startupReadyTimeout:  30 * time.Second,
		stderr:               os.Stderr,
		nodeSelector:         scheduling.nodeSelector,
		tolerations:          scheduling.tolerations,
		affinity:             scheduling.affinity,
		priorityClassName:    scheduling.priorityClassName,
		codexAuthSecret:      strings.TrimSpace(os.Getenv("GC_K8S_CODEX_AUTH_SECRET")),
		providerNames:        make(map[string]string),
	}, nil
}

func parseSchedulingEnv() (schedulingFields, error) {
	var scheduling schedulingFields
	if v := os.Getenv("GC_K8S_NODE_SELECTOR"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.nodeSelector); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_NODE_SELECTOR: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_TOLERATIONS"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.tolerations); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_TOLERATIONS: %w", err)
		}
	}
	if v := os.Getenv("GC_K8S_AFFINITY"); v != "" {
		if err := json.Unmarshal([]byte(v), &scheduling.affinity); err != nil {
			return schedulingFields{}, fmt.Errorf("parsing GC_K8S_AFFINITY: %w", err)
		}
	}
	scheduling.priorityClassName = os.Getenv("GC_K8S_PRIORITY_CLASS_NAME")
	return scheduling, nil
}

// newProviderWithOps creates a provider with a custom k8sOps (for testing).
func newProviderWithOps(ops k8sOps) *Provider {
	return &Provider{
		ops:                  ops,
		namespace:            "test-ns",
		image:                "test-image:latest",
		managedServiceHost:   podManagedDoltHost,
		managedServicePort:   podManagedDoltPort,
		cpuRequest:           "500m",
		memRequest:           "1Gi",
		cpuLimit:             "2",
		memLimit:             "4Gi",
		startupDialogTimeout: time.Millisecond,
		startupReadyTimeout:  30 * time.Second,
		stderr:               io.Discard,
		providerNames:        make(map[string]string),
	}
}

// Start creates a new K8s pod running a tmux session with the agent command.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.image == "" {
		return fmt.Errorf("starting session %q: GC_K8S_IMAGE is required", name)
	}
	podName := SanitizeName(name)
	label := SanitizeLabel(name)

	// Check for existing pod (any phase).
	existing, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err == nil && len(existing) > 0 {
		pod := &existing[0]
		if pod.Status.Phase == corev1.PodRunning {
			// Stale-pod detection: is the session's tmux server alive?
			alive, definitive, probeErr := p.probeTmuxLiveness(ctx, pod)
			if alive {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionExists, name, pod.Name)
			}
			// tmux not answering — but if the pod is young, workspace init may
			// still be blocking the tmux server from starting. Don't delete
			// pods that are still within the startup window.
			if time.Since(pod.CreationTimestamp.Time) < startupGracePeriod {
				return fmt.Errorf("%w: session %q (pod: %s)", runtime.ErrSessionInitializing, name, pod.Name)
			}
			if !definitive {
				// Past the grace period the next statement deletes this pod.
				// Only a probe that actually ran inside the container may
				// authorize that: an apiserver or kubelet transport failure
				// says nothing about tmux, and treating it as a negative
				// destroys a live agent's box and the work in it. Report the
				// established "I could not tell" signal so the caller defers
				// instead of recreating.
				return fmt.Errorf("%w: tmux liveness probe for session %q (pod: %s) could not answer: %w",
					runtime.ErrRuntimeUnavailable, name, pod.Name, probeErr)
			}
			// Stale pod — tmux definitively dead and past grace period, recreate.
		}
		// Clean up existing pod.
		_ = p.ops.deletePod(ctx, pod.Name, 5)
		_ = waitForDeletion(ctx, p.ops, pod.Name, 30*time.Second)
	}

	// Build and create pod.
	pod, err := buildPod(name, cfg, p)
	if err != nil {
		return fmt.Errorf("building pod for session %q: %w", name, err)
	}
	_, err = p.ops.createPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("creating pod for session %q: %w", name, err)
	}

	// cleanup deletes the pod on any startup failure after creation.
	// Uses a fresh background context so cleanup succeeds even if the
	// original ctx was canceled (which is the common failure path).
	cleanup := func(_ string) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.ops.deletePod(cleanupCtx, podName, 5)
	}

	ctrlCity := cfg.Env["GC_CITY"]

	if !p.prebaked {
		// Stage files via init container if needed.
		if needsStaging(cfg, ctrlCity) {
			if err := stageFiles(ctx, p.ops, podName, cfg, ctrlCity, p.stderr); err != nil {
				cleanup("staging failed")
				return fmt.Errorf("staging files for session %q: %w", name, err)
			}
		}
	}

	// Wait for main container to be running.
	if err := waitForPodRunning(ctx, p.ops, podName, 120*time.Second); err != nil {
		cleanup("pod not running")
		return fmt.Errorf("waiting for pod %q: %w", podName, err)
	}

	if !p.prebaked {
		// Initialize the city inside the pod.
		if ctrlCity != "" {
			if err := initCityInPod(ctx, p.ops, podName, ctrlCity); err != nil {
				fmt.Fprintf(p.stderr, "gc: warning: initCityInPod for %s: %v\n", podName, err) //nolint:errcheck
			}
		}

		// Signal entrypoint to proceed.
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"touch", "/workspace/.gc-workspace-ready"}, nil); err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: touch .gc-workspace-ready in %s: %v\n", podName, err) //nolint:errcheck
		}
	}

	// Ensure .beads/ inside the pod. This remains warning-only so older staged
	// or prebaked workspaces can self-heal instead of failing session startup.
	podWorkDir := projectedPodWorkDir(cfg)
	if err := initBeadsInPod(ctx, p.ops, podName, cfg, podWorkDir, p.managedServiceHost, p.managedServicePort); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: initBeadsInPod for %s: %v\n", podName, err) //nolint:errcheck
	}

	// Wait for tmux session.
	if err := waitForTmux(ctx, p.ops, podName, 60*time.Second); err != nil {
		cleanup("tmux not ready")
		return fmt.Errorf("waiting for tmux in pod %q: %w", podName, err)
	}

	// Enable pane logging + run session setup (shared with the relaunch tail).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	// K8s sessions run the same interactive CLI contract as the local tmux
	// provider, but the tmux server lives inside the worker pod.  Handle known
	// startup dialogs through that in-pod carrier before the reconciler marks
	// creation complete; otherwise a first-run Codex workspace-trust modal can
	// leave the pane dead while the pod itself remains alive.
	if runtime.ShouldAcceptStartupDialogs(cfg) {
		if err := p.acceptStartupDialogs(ctx, name); err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: startup dialogs for %s: %v\n", podName, err) //nolint:errcheck
		}
	}

	// A live tmux server is not the same thing as a ready interactive agent.
	// Codex can still be showing its workspace-trust screen when the server
	// appears; sending the startup nudge in that window lets its Escape key
	// cancel the CLI before the first turn. Wait for the provider's declared
	// prompt after dialog handling, before any nudge reaches the pane.
	if err := p.waitForReadyPrompt(ctx, name, cfg); err != nil {
		cleanup("agent prompt not ready")
		return fmt.Errorf("waiting for ready prompt for session %q: %w", name, err)
	}

	requiresPostStartLiveness := k8sRequiresPostStartLiveness(cfg)

	// Post-start liveness check: verify interactive sessions survived startup.
	// Agents that fail immediately (e.g. --resume with a stale session key)
	// exit within a second. A brief settle lets us detect this before
	// returning success to the reconciler, which triggers recordWakeFailure
	// and the crash-loop recovery (clear session_key, bump continuation_epoch).
	//
	// Some configured commands are intentionally one-turn processes. Those
	// should return from Start after the first tmux appearance and let normal
	// session reconciliation observe completion, rather than converting clean
	// command exit into startup failure.
	if requiresPostStartLiveness && p.postStartSettle > 0 {
		timer := time.NewTimer(p.postStartSettle)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			cleanup("post-start settle canceled")
			return fmt.Errorf("waiting for post-start settle for session %q: %w", name, ctx.Err())
		case <-timer.C:
		}
	}
	if requiresPostStartLiveness {
		_, tmuxErr := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if tmuxErr != nil {
			cleanup("session died immediately after startup")
			return fmt.Errorf("%w: session %q died immediately after startup: %w",
				runtime.ErrSessionDiedDuringStartup, name, tmuxErr)
		}
	}

	p.setProviderName(name, cfg.ProviderName)

	// Send initial nudge if configured (matches tmux adapter step 6).
	if cfg.Nudge != "" {
		if err := p.Nudge(name, runtime.TextContent(cfg.Nudge)); err != nil {
			cleanup("initial nudge failed")
			return fmt.Errorf("sending initial nudge for session %q: %w", name, err)
		}
	}

	return nil
}

func (p *Provider) acceptStartupDialogs(ctx context.Context, name string) error {
	timeout := p.startupDialogTimeout
	if timeout <= 0 {
		timeout = runtime.StartupDialogTimeout()
	}
	return p.acceptStartupDialogsWithTimeout(ctx, name, timeout)
}

func (p *Provider) acceptStartupDialogsWithTimeout(ctx context.Context, name string, timeout time.Duration) error {
	return runtime.AcceptStartupDialogsWithTimeout(
		ctx,
		timeout,
		func(lines int) (string, error) {
			return p.carrier().Peek(ctx, name, lines)
		},
		func(keys ...string) error {
			return p.carrier().SendKeys(ctx, name, keys...)
		},
	)
}

const startupReadyPollInterval = 250 * time.Millisecond

// A provider can briefly render its normal composer before a first-run modal
// arrives. Probe startup dialogs during readiness with a short budget so a
// late modal is handled without turning every readiness poll into a full
// startup-dialog wait.
const startupDialogProbeTimeout = 100 * time.Millisecond

func (p *Provider) waitForReadyPrompt(ctx context.Context, name string, cfg runtime.Config) error {
	prefix := strings.TrimSpace(cfg.ReadyPromptPrefix)
	if prefix == "" {
		if cfg.ReadyDelayMs <= 0 {
			return nil
		}
		timer := time.NewTimer(time.Duration(cfg.ReadyDelayMs) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}

	timeout := p.startupReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, err := p.carrier().Peek(ctx, name, 120)
		if err == nil {
			if readyPromptVisible(content, prefix) {
				return nil
			}
			if runtime.ShouldAcceptStartupDialogs(cfg) {
				if err := p.acceptStartupDialogsWithTimeout(ctx, name, startupDialogProbeTimeout); err != nil {
					lastErr = err
				}
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("prompt %q did not appear within %s", prefix, timeout)
		}
		timer := time.NewTimer(startupReadyPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func readyPromptVisible(content, prefix string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.ReplaceAll(line, "\u00a0", " ")
		line = strings.TrimLeft(line, " \t│┃")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		// Codex's trust screen uses the same composer glyph for its numbered
		// selection row. It is not the ready composer and must not receive the
		// provider nudge's Escape-then-Enter submit sequence.
		if len(rest) >= 2 && rest[0] >= '0' && rest[0] <= '9' && rest[1] == '.' {
			continue
		}
		return true
	}
	return false
}

// runPodPostLaunchSetup enables pane logging and runs session_setup and
// session_setup_script inside the pod, best-effort. Shared by Start (after the
// entrypoint launches the agent) and Relaunch (after the respawn). k8s does not
// run SessionLive (RunLive is a no-op), matching the pre-un-weld behavior.
func (p *Provider) runPodPostLaunchSetup(ctx context.Context, podName string, cfg runtime.Config) {
	// Enable pane logging for diagnostics.
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "pipe-pane", "-t", tmuxSession, "-o", "cat >> /tmp/agent-output.log"}, nil)

	// Run session_setup commands inside the pod.
	for _, cmd := range cfg.SessionSetup {
		if cmd == "" {
			continue
		}
		_, _ = p.ops.execInPod(ctx, podName, "agent",
			[]string{"sh", "-c", cmd}, nil)
	}

	// Run session_setup_script.
	if cfg.SessionSetupScript != "" {
		script, err := os.ReadFile(cfg.SessionSetupScript)
		if err != nil {
			fmt.Fprintf(p.stderr, "gc: warning: reading session_setup_script %q for %s: %v\n", cfg.SessionSetupScript, podName, err) //nolint:errcheck
		} else {
			_, _ = p.ops.execInPod(ctx, podName, "agent",
				[]string{"sh"}, strings.NewReader(string(script)))
		}
	}
}

// Relaunch re-launches the agent inside the already-running (warm) pod without
// recreating it: it respawns the in-pod tmux "main" pane (respawn-pane -k) with
// the (possibly changed) command over execInPod, then re-runs the post-launch
// setup tail. The pod stays warm via the entrypoint's `sleep infinity`, so a
// launch-only config change reaches the live pod without a full reprovision —
// the k8s half of the runtime/transport un-weld (B3a, mirroring tmux B1 / ssh).
//
// The pod must be Running with a live tmux "main" session, else
// [runtime.ErrSessionNotFound] (the reconciler decides whether to Start fresh —
// it does NOT recreate the pod here). Staging, city/beads init, and PreStart are
// NOT re-run here (k8s treats PreStart as provision-half); env is provision-half
// too (set in the pod spec at create time, not re-injected — respawn-pane carries
// no env). NOTE: tmux diverges — as of the relaunch pre_start fix it re-runs
// PreStart on Relaunch (launch-half), while k8s and ssh intentionally do not.
//
// CAVEAT (unverified on a real cluster — see the B3 design doc): for
// LINUX_USERNAME pods the entrypoint runs tmux under `su - <user>`, so the
// respawn is su-wrapped to reach that user's tmux socket; and if the in-pod tmux
// server itself died (not just the agent), respawn-pane fails and Relaunch
// returns ErrSessionNotFound so the reconciler reprovisions.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: session %q (no running pod to relaunch into)", runtime.ErrSessionNotFound, name)
	}
	// The tmux server + "main" session must be alive to respawn into; a dead
	// session means the box is not warm enough — reprovision, don't respawn.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
		return fmt.Errorf("%w: session %q (pod %s has no live tmux session)", runtime.ErrSessionNotFound, name, podName)
	}

	// Respawn the agent in the warm "main" session.
	if _, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"sh", "-c", buildRespawnCommand(cfg)}, nil); err != nil {
		return fmt.Errorf("k8s relaunch %q: respawn-pane: %w", name, err)
	}

	// Re-run the post-launch setup tail (pipe-pane logging + session_setup[/script]).
	p.runPodPostLaunchSetup(ctx, podName, cfg)

	// Respawned agents have the same interactive startup boundary as freshly
	// created pods. Do not nudge a warm carrier until its declared prompt is
	// visible; tmux liveness alone only proves that the server survived.
	if err := p.waitForReadyPrompt(ctx, name, cfg); err != nil {
		return fmt.Errorf("waiting for ready prompt after relaunch for session %q: %w", name, err)
	}

	// Post-relaunch liveness: detect an agent that dies immediately.
	if k8sRequiresPostStartLiveness(cfg) {
		if p.postStartSettle > 0 {
			timer := time.NewTimer(p.postStartSettle)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return fmt.Errorf("k8s relaunch %q: %w", name, ctx.Err())
			case <-timer.C:
			}
		}
		if _, err := p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil); err != nil {
			return fmt.Errorf("%w: session %q died immediately after relaunch: %w",
				runtime.ErrSessionDiedDuringStartup, name, err)
		}
	}

	p.setProviderName(name, cfg.ProviderName)
	if cfg.Nudge != "" {
		if err := p.Nudge(name, runtime.TextContent(cfg.Nudge)); err != nil {
			return fmt.Errorf("sending relaunch nudge for session %q: %w", name, err)
		}
	}
	return nil
}

func k8sRequiresPostStartLiveness(cfg runtime.Config) bool {
	if cfg.Lifecycle == runtime.LifecycleOneShot {
		return false
	}
	return runtime.HasManagedStartupHints(cfg)
}

// Stop deletes the pod(s) for the named session. Missing/not-found is
// idempotent (no error: the session is genuinely gone), but a transport
// failure surfaces. A list failure is "unknown state", NOT "session gone" —
// swallowing it would let the seam adapter (Runtime.Teardown → Stop) drop
// tracking while pods and their PVCs keep running untracked, leaking the most
// expensive runtime. Delete errors are joined for the same reason; only a
// genuine Kubernetes NotFound (the pod raced to gone) is treated as idempotent.
// This mirrors the ssh provider's Stop discrimination.
func (p *Provider) Stop(name string) error {
	ctx := context.Background()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			p.deleteProviderName(name)
			return nil // session genuinely gone
		}
		return fmt.Errorf("k8s stop %q: listing pods: %w", name, err)
	}
	var errs []error
	for i := range pods {
		if delErr := p.ops.deletePod(ctx, pods[i].Name, 5); delErr != nil && !apierrors.IsNotFound(delErr) {
			errs = append(errs, fmt.Errorf("deleting pod %q: %w", pods[i].Name, delErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("k8s stop %q: %w", name, errors.Join(errs...))
	}
	p.deleteProviderName(name)
	return nil
}

// StopIfInstanceToken deletes only the carrier incarnation whose direct
// PodSpec GC_INSTANCE_TOKEN matches expectedToken. The token check covers the
// tmux-dead state, where tmux metadata is unavailable; the UID precondition
// closes the list/verify/delete race where the pod is replaced under the same
// Kubernetes name after verification.
func (p *Provider) StopIfInstanceToken(name, expectedToken string) error {
	ctx := context.Background()
	name = strings.TrimSpace(name)
	expectedToken = strings.TrimSpace(expectedToken)
	if name == "" || expectedToken == "" {
		return fmt.Errorf("%w: session name and expected token are required", runtime.ErrInstanceTokenMismatch)
	}

	deletable, ok := p.ops.(podUIDDeleteOps)
	if !ok {
		return fmt.Errorf("%w: provider cannot enforce pod UID deletion precondition", runtime.ErrInstanceTokenMismatch)
	}

	pods, err := p.ops.listPods(ctx, "gc-session="+SanitizeLabel(name), "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			p.deleteProviderName(name)
			return nil
		}
		return fmt.Errorf("k8s stop %q: listing pods: %w", name, err)
	}
	if len(pods) == 0 {
		p.deleteProviderName(name)
		return nil
	}

	// Verify every candidate before deleting any of them. A label collision or
	// a terminating old pod must never turn a partially verified batch into a
	// name-only delete of another incarnation.
	for i := range pods {
		actualToken, present := podIdentityValue(&pods[i], "GC_INSTANCE_TOKEN")
		if !present || strings.TrimSpace(actualToken) != expectedToken || pods[i].UID == "" {
			return fmt.Errorf("%w: session %q carrier identity does not match", runtime.ErrInstanceTokenMismatch, name)
		}
	}

	for i := range pods {
		if delErr := deletable.deletePodWithUID(ctx, pods[i].Name, pods[i].UID, 5); delErr != nil {
			if apierrors.IsNotFound(delErr) {
				continue // the verified carrier disappeared; nothing new was deleted
			}
			if apierrors.IsConflict(delErr) {
				return fmt.Errorf("%w: session %q carrier was replaced", runtime.ErrInstanceTokenMismatch, name)
			}
			return fmt.Errorf("deleting pod %q: %w", pods[i].Name, delErr)
		}
	}
	p.deleteProviderName(name)
	return nil
}

// Interrupt sends Ctrl-C to the tmux session inside the pod.
func (p *Provider) Interrupt(name string) error {
	_ = p.carrier().Interrupt(context.Background(), name) // best-effort
	return nil
}

// IsRunning reports whether the session has a running pod with a live tmux session.
func (p *Provider) IsRunning(name string) bool {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return false
	}
	// Pod Running + tmux session alive.
	_, err = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
	return err == nil
}

// IsAttached reports whether a user terminal is connected to the tmux
// session inside the pod.
func (p *Provider) IsAttached(name string) bool {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return false
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}, nil)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "1"
}

// Attach shells out to kubectl exec -it for full TTY passthrough.
func (p *Provider) Attach(name string) error {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return fmt.Errorf("attach: no running pod for session %q", name)
	}

	args := []string{}
	if p.k8sContext != "" {
		args = append(args, "--context", p.k8sContext)
	}
	args = append(args, "-n", p.namespace, "exec", "-it", podName, "--",
		"tmux", "attach", "-t", tmuxSession)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ProcessAlive checks if the named processes are running inside the pod.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	if len(processNames) == 0 {
		return true
	}
	ctx := context.Background()
	label := SanitizeLabel(name)

	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil || len(pods) == 0 {
		return false
	}
	pod := &pods[0]

	// Check deletionTimestamp — pod in graceful shutdown is not alive.
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, pname := range processNames {
		_, err := p.ops.execInPod(ctx, pod.Name, "agent",
			[]string{"pgrep", "-f", pname}, nil)
		if err == nil {
			return true
		}
	}
	return false
}

// Nudge types a message into the tmux session followed by Enter.
// Uses -l (literal mode) so tmux key names in the message text are not
// interpreted as keystrokes. Content blocks are flattened to text.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	return p.carrier().Nudge(context.Background(), name, content)
}

// SendKeys sends bare keystrokes to the tmux session. Best-effort on a
// missing session (contract: no-op), but a genuine transport failure to a
// live pod is propagated (#4389).
func (p *Provider) SendKeys(name string, keys ...string) error {
	if err := p.carrier().SendKeys(context.Background(), name, keys...); err != nil && !errors.Is(err, runtime.ErrSessionNotFound) {
		return err
	}
	return nil
}

// RunLive re-applies session_live commands. Not yet supported for K8s.
func (p *Provider) RunLive(_ string, _ runtime.Config) error {
	return nil
}

// SetMeta stores a key-value pair in the tmux environment.
func (p *Provider) SetMeta(name, key, value string) error {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, key, value}, nil)
	return nil
}

// GetMeta retrieves a metadata value from the tmux environment.
func (p *Provider) GetMeta(name, key string) (string, error) {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return "", nil
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "show-environment", "-t", tmuxSession, key}, nil)
	if err != nil {
		// Pod-provided identity variables are inherited by the tmux server's
		// global environment, while SetMeta writes session-scoped values. Read
		// the global environment as a fallback so startup identity checks can
		// match a freshly-created pod before the first session-scoped metadata
		// write lands.
		output, err = p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "show-environment", "-g", key}, nil)
		if err != nil {
			// The agent's tmux server can die while the carrier pod remains
			// Running (for example, a provider restart after Codex exits during
			// startup). Identity written into the pod environment is still
			// authoritative in that state, and lets the controller attribute the
			// runtime to its closed session bead so the closed-bead reaper can
			// delete the carrier instead of leaking it.
			if value, ok := p.podIdentityEnv(ctx, podName, key); ok {
				return value, nil
			}
			return "", nil
		}
	}
	output = strings.TrimSpace(output)
	// tmux output: "KEY=VALUE" (set), "-KEY" (unset).
	if strings.HasPrefix(output, "-") {
		return "", nil // explicitly unset
	}
	if _, val, ok := strings.Cut(output, "="); ok {
		return val, nil
	}
	return "", nil
}

// podIdentityEnv returns only direct, non-SecretRef identity values from the
// agent container's PodSpec. It is deliberately a narrow fallback for carrier
// identity, not a general environment reader: Kubernetes Secret-backed values
// are never resolved here, and arbitrary pod configuration is not exposed via
// the runtime metadata API.
func (p *Provider) podIdentityEnv(ctx context.Context, podName, key string) (string, bool) {
	if !isPodIdentityKey(key) {
		return "", false
	}
	pod, err := p.ops.getPod(ctx, podName)
	if err != nil || pod == nil {
		return "", false
	}
	return podIdentityValue(pod, key)
}

func isPodIdentityKey(key string) bool {
	switch key {
	case "GC_SESSION_ID", "GC_SESSION_NAME", "GC_INSTANCE_TOKEN":
		return true
	default:
		return false
	}
}

func podIdentityValue(pod *corev1.Pod, key string) (string, bool) {
	if pod == nil || !isPodIdentityKey(key) {
		return "", false
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != "agent" {
			continue
		}
		for _, env := range container.Env {
			if env.Name == key && env.ValueFrom == nil {
				return env.Value, true
			}
		}
	}
	return "", false
}

// RemoveMeta removes a metadata key from the tmux environment.
func (p *Provider) RemoveMeta(name, key string) error {
	ctx := context.Background()
	podName, err := p.findPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	_, _ = p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "set-environment", "-t", tmuxSession, "-u", key}, nil)
	return nil
}

// Peek captures the last N lines of tmux pane output (best-effort: empty on failure).
func (p *Provider) Peek(name string, lines int) (string, error) {
	out, _ := p.carrier().Peek(context.Background(), name, lines) // best-effort
	return out, nil
}

// ListRunning returns names of all running sessions with the given prefix.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	ctx := context.Background()
	pods, err := p.ops.listPods(ctx, "app=gc-agent", "status.phase=Running")
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pods {
		pod := &pods[i]
		// Prefer annotation (raw name) over label (sanitized).
		name := pod.Annotations["gc-session-name"]
		if name == "" {
			name = pod.Labels["gc-session"]
		}
		if name == "" {
			continue
		}
		if prefix == "" || strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// GetLastActivity returns the time of the last I/O in the tmux session.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return time.Time{}, nil
	}
	output, err := p.ops.execInPod(ctx, podName, "agent",
		[]string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_activity}"}, nil)
	if err != nil {
		return time.Time{}, nil
	}
	epoch := strings.TrimSpace(output)
	if epoch == "" {
		return time.Time{}, nil
	}
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return time.Time{}, nil
	}
	return time.Unix(secs, 0), nil
}

// ClearScrollback clears the tmux scrollback buffer (best-effort).
func (p *Provider) ClearScrollback(name string) error {
	_ = p.carrier().ClearScrollback(context.Background(), name) // best-effort
	return nil
}

// Capabilities reports K8s provider capabilities. The K8s provider
// supports activity tracking via tmux session_activity but does not
// support attachment detection from the controller host.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	return runtime.ProviderCapabilities{
		CanReportActivity: true,
	}
}

// SleepCapability reports that k8s sessions can participate in timed-only
// idle sleep. The controller cannot observe attachment state from the host.
func (p *Provider) SleepCapability(string) runtime.SessionSleepCapability {
	return runtime.SessionSleepCapabilityTimedOnly
}

// CopyTo copies a local file/directory into the pod via tar.
func (p *Provider) CopyTo(name, src, relDst string) error {
	ctx := context.Background()
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return nil // best-effort
	}
	dst := "/workspace"
	if relDst != "" {
		dst = "/workspace/" + relDst
	}
	return copyToPod(ctx, p.ops, podName, "agent", src, dst)
}

// --- Internal helpers ---

// livenessProbeAttempts is how many times probeTmuxLiveness will ask before
// reporting that it could not tell. The exec path through the apiserver and
// kubelet flakes independently of the pod, and a single blip is not evidence
// about tmux; two unanswered attempts in a row are at least a pattern.
const livenessProbeAttempts = 2

// probeTmuxLiveness asks whether the session's tmux server is alive inside pod.
//
// It returns three-valued, not two: alive, and whether the answer is
// definitive. A definitive answer means something actually established the
// fact — either the pod's own status says the agent container is not running,
// or `tmux has-session` ran inside it and exited. An indefinite answer means
// the exec never got there (SPDY dial failure, apiserver error, stream timeout,
// canceled context), which says nothing at all about tmux.
//
// Collapsing those two into "not alive" is how a transport flake becomes a
// deleted pod. Callers that act destructively on a negative must require
// definitive; callers that only defer may ignore it.
func (p *Provider) probeTmuxLiveness(ctx context.Context, pod *corev1.Pod) (alive, definitive bool, err error) {
	// The pod's status is a second, independent channel, and it settles the
	// case the exec cannot: a pod whose phase is Running but whose agent
	// container is not (crash loop, OOM, terminated) answers every exec with an
	// apiserver-level error that is indistinguishable from a flake. Reading
	// that as "I could not tell" would leave a genuinely broken pod in place
	// forever. The container not running IS a definitive tmux negative, and it
	// comes from the list we already did rather than from the connection that
	// is in doubt.
	if running, known := agentContainerRunning(pod); known && !running {
		return false, true, fmt.Errorf("agent container in pod %s is not running", pod.Name)
	}
	podName := pod.Name
	for attempt := 0; attempt < livenessProbeAttempts; attempt++ {
		_, err = p.ops.execInPod(ctx, podName, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if err == nil {
			return true, true, nil
		}
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// The command ran in the container and reported no session. That
			// is a real tmux negative — the same discrimination Provider.Exec
			// already draws between an exit status and a transport failure.
			return false, true, err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false, false, err
}

// agentContainerRunning reports whether the pod's agent container is running,
// and whether the pod status said anything about it at all. A status that has
// not been populated yet is not evidence either way.
func agentContainerRunning(pod *corev1.Pod) (running, known bool) {
	if pod == nil {
		return false, false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" {
			return cs.State.Running != nil, true
		}
	}
	return false, false
}

// findRunningPod finds a running pod by session label.
// carrier returns the tmux carrier that drives this provider's sessions over
// the pod exec connection ([Provider.Exec]). The in-box tmux session is always
// tmuxSession ("main").
func (p *Provider) carrier() runtime.Carrier {
	return runtime.NewTmuxCarrierWithSubmitKeys(p, tmuxSession, p.submitKeys)
}

func (p *Provider) setProviderName(name, provider string) {
	p.providerMu.Lock()
	defer p.providerMu.Unlock()
	if p.providerNames == nil {
		p.providerNames = make(map[string]string)
	}
	p.providerNames[name] = provider
}

func (p *Provider) deleteProviderName(name string) {
	p.providerMu.Lock()
	defer p.providerMu.Unlock()
	delete(p.providerNames, name)
}

func (p *Provider) submitKeys(name string) []string {
	p.providerMu.RLock()
	provider := p.providerNames[name]
	p.providerMu.RUnlock()
	return runtime.NudgeSubmitKeySequenceForProvider(provider)
}

// Exec implements [runtime.ExecProvider]: it runs argv inside the session
// pod's "agent" container and returns the command's standard output and exit
// code (execInPod returns stdout; stderr is folded into err on a transport
// failure). A command that runs but exits non-zero yields that code with a nil
// error; only a transport failure (no running pod, stream error) yields err.
// This is the connection the tmux carrier drives the session through.
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	podName, err := p.findRunningPod(ctx, name)
	if err != nil {
		return nil, -1, fmt.Errorf("k8s exec %q: %w", name, err)
	}
	out, err := p.ops.execInPod(ctx, podName, "agent", argv, nil)
	if err != nil {
		var exitErr execerr.ExitError
		if errors.As(err, &exitErr) && exitErr.Exited() {
			// Ran and exited non-zero: the command's own result, not a
			// transport failure (per the ExecProvider contract).
			return []byte(out), exitErr.ExitStatus(), nil
		}
		return []byte(out), -1, err
	}
	return []byte(out), 0, nil
}

// findRunningPod resolves the running pod for name. A missing pod (scaled
// down, evicted, never provisioned) is reported as [runtime.ErrSessionNotFound]
// so callers can distinguish "session is gone" from a genuine transport
// failure reaching a pod that does exist — the same distinction Relaunch
// already draws at its own call site.
func (p *Provider) findRunningPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "status.phase=Running")
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("%w: no running pod for session %q", runtime.ErrSessionNotFound, name)
	}
	return pods[0].Name, nil
}

// findPod finds a pod by session label (any phase).
func (p *Provider) findPod(ctx context.Context, name string) (string, error) {
	label := SanitizeLabel(name)
	pods, err := p.ops.listPods(ctx, "gc-session="+label, "")
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", fmt.Errorf("no pod for session %q", name)
	}
	return pods[0].Name, nil
}

// waitForDeletion waits for a pod to be deleted.
func waitForDeletion(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.getPod(ctx, name)
		if err != nil {
			return nil // gone
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not deleted after %s", name, timeout)
}

// waitForPodRunning waits for the pod to reach Running phase.
func waitForPodRunning(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		pod, err := ops.getPod(ctx, name)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("pod %s failed", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("pod %s not running after %s", name, timeout)
}

// waitForTmux waits for the tmux session to be available inside the pod.
func waitForTmux(ctx context.Context, ops k8sOps, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := ops.execInPod(ctx, name, "agent",
			[]string{"tmux", "has-session", "-t", tmuxSession}, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("tmux session not ready in pod %s after %s", name, timeout)
}

// initCityInPod copies the city directory and runs gc init inside the pod.
// The staged city carries the canonical hosted-Dolt identity, but the session
// env may only carry the connection target. Supply the project/database
// identity from the staged metadata when the target is present; otherwise
// clear the hosted-Dolt env for this filesystem-only scaffold operation. This
// keeps gc init from rejecting a valid staged city before initBeadsInPod strips
// the controller-side identity for the pod's own server handshake.
func initCityInPod(ctx context.Context, ops k8sOps, podName, ctrlCity string) error {
	// Copy city dir (excluding .gc/) into the pod.
	if err := copyDirToPod(ctx, ops, podName, "agent", ctrlCity, "/tmp/city-src"); err != nil {
		return err
	}
	// Run gc init --from with GC_DOLT=skip so gc init does not attempt to
	// start a local Dolt server. Pod sessions consume the projected GC_DOLT_*
	// connection target through env; they do not rewrite canonical .beads files.
	// Hosted init validation still requires the project/database identity, so
	// recover those non-secret fields from the staged canonical metadata when
	// the projected target is complete. A filesystem-only scaffold must clear
	// the partial target instead of making gc init reject the copied city.
	initScript := `set -eu
# stageFiles may have copied the controller's scoped bead stores into the
# worker workspace. Those stores contain read-only formula files (often
# dereferenced from pack symlinks), and gc init --from must create the
# worker's own writable scoped stores instead of trying to rewrite them.
find /workspace -type d -name .beads -prune -exec rm -rf -- {} +
PROJECT_ID="${GC_BEADS_PROJECT_ID:-}"
DOLT_DATABASE="${GC_DOLT_DATABASE:-}"
if [ -f /tmp/city-src/.beads/metadata.json ]; then
  PROJECT_ID="$(python3 -c 'import json,sys; print(str(json.load(open(sys.argv[1])).get("project_id", "")).strip())' /tmp/city-src/.beads/metadata.json 2>/dev/null || true)"
  DOLT_DATABASE="$(python3 -c 'import json,sys; print(str(json.load(open(sys.argv[1])).get("dolt_database", "")).strip())' /tmp/city-src/.beads/metadata.json 2>/dev/null || true)"
fi
if [ -n "${GC_DOLT_HOST:-}" ] && [ -n "${GC_DOLT_PORT:-}" ] && [ -n "$DOLT_DATABASE" ] && [ -n "$PROJECT_ID" ]; then
  GC_DOLT=skip GC_DOLT_DATABASE="$DOLT_DATABASE" GC_BEADS_PROJECT_ID="$PROJECT_ID" gc init --from /tmp/city-src /workspace --no-start --skip-provider-readiness
else
  env -u GC_DOLT_HOST -u GC_DOLT_PORT -u GC_DOLT_USER -u GC_DOLT_DATABASE -u GC_BEADS_PROJECT_ID GC_DOLT=skip gc init --from /tmp/city-src /workspace --no-start --skip-provider-readiness
fi`
	_, err := ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", initScript}, nil)
	if err != nil {
		return err
	}
	// Clean up.
	_, _ = ops.execInPod(ctx, podName, "agent",
		[]string{"rm", "-rf", "/tmp/city-src"}, nil)
	return nil
}

// initBeadsInPod ensures the pod workspace has usable .beads state. It keeps
// the older warning-only self-heal behavior for prebaked or older staged
// workspaces by patching existing metadata and bootstrapping missing state.
func initBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, workDir, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	doltHost := projected["GC_DOLT_HOST"]
	doltPort := projected["GC_DOLT_PORT"]
	storeRoot := projectedPodStoreRoot(cfg, workDir)
	prefix := strings.TrimSpace(cfg.Env["GC_BEADS_PREFIX"])

	portNum, err := strconv.Atoi(doltPort)
	if err != nil {
		return fmt.Errorf("invalid projected GC_DOLT_PORT %q: %w", doltPort, err)
	}
	patchJSON, err := json.Marshal(map[string]any{
		"dolt_server_host": doltHost,
		"dolt_server_port": portNum,
	})
	if err != nil {
		return fmt.Errorf("marshaling beads patch: %w", err)
	}
	patchB64 := base64.StdEncoding.EncodeToString(patchJSON)
	prefixB64 := base64.StdEncoding.EncodeToString([]byte(prefix))
	storeRootB64 := base64.StdEncoding.EncodeToString([]byte(storeRoot))

	patchCmd := fmt.Sprintf(
		`WD=$(echo '%s' | base64 -d) && cd "$WD" && PATCH=$(echo '%s' | base64 -d) && `+
			`if [ -f .beads/metadata.json ]; then `+
			`python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.argv[1]); m.update(p); m.pop('project_id', None); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)" "$PATCH" 2>/dev/null || `+
			`printf '%%s' "$PATCH" | python3 -c "import json,sys; `+
			`m=json.load(open('.beads/metadata.json')); `+
			`p=json.loads(sys.stdin.read()); m.update(p); m.pop('project_id', None); `+
			`json.dump(m,open('.beads/metadata.json','w'),indent=2)"; `+
			`else PREFIX=$(echo '%s' | base64 -d) && `+
			`[ -n "$PREFIX" ] || { echo 'missing projected GC_BEADS_PREFIX' >&2; exit 1; } && `+
			`DOLT_HOST=$(echo '%s' | base64 -d) && `+
			`DOLT_PORT=$(echo '%s' | base64 -d) && `+
			`yes | BEADS_DIR="$WD/.beads" bd init --server --server-host "$DOLT_HOST" --server-port "$DOLT_PORT" -p "$PREFIX" --skip-hooks --skip-agents; fi`,
		storeRootB64, patchB64, prefixB64,
		base64.StdEncoding.EncodeToString([]byte(doltHost)),
		base64.StdEncoding.EncodeToString([]byte(doltPort)),
	)
	_, err = ops.execInPod(ctx, podName, "agent", []string{"sh", "-c", patchCmd}, nil)
	return err
}

// verifyBeadsInPod confirms that canonical tracked .beads files are already
// present in the mounted workspace for bd-backed sessions. It intentionally
// does not create or rewrite .beads state inside the pod.
//
//nolint:unparam // tests exercise this helper through the canonical managed service constants.
func verifyBeadsInPod(ctx context.Context, ops k8sOps, podName string, cfg runtime.Config, storeRoot, managedServiceHost, managedServicePort string) error {
	projected, err := projectedPodDoltEnv(cfg.Env, managedServiceHost, managedServicePort)
	if err != nil {
		return err
	}
	if len(projected) == 0 {
		return nil
	}
	_, err = ops.execInPod(ctx, podName, "agent", []string{
		"sh", "-c",
		`cd "$1" && test -f .beads/metadata.json && test -f .beads/config.yaml`,
		"sh", storeRoot,
	}, nil)
	if err != nil {
		return fmt.Errorf("canonical .beads files missing or unreadable at %s: %w", storeRoot, err)
	}
	return nil
}

func buildRESTConfig(k8sContext string) (*rest.Config, error) {
	// Try in-cluster first.
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	// Fall back to kubeconfig.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if k8sContext != "" {
		overrides.CurrentContext = k8sContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func managedServiceAlias() (string, string, error) {
	host := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_HOST"))
	port := strings.TrimSpace(os.Getenv("GC_K8S_DOLT_PORT"))
	switch {
	case host == "" && port == "":
		return podManagedDoltHost, podManagedDoltPort, nil
	case host == "" || port == "":
		return "", "", fmt.Errorf("requires both GC_K8S_DOLT_HOST and GC_K8S_DOLT_PORT when either is set")
	default:
		return host, port, nil
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
