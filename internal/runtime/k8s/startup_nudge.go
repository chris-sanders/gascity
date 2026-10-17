package k8s

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

const startupNudgeDebounce = 500 * time.Millisecond

const (
	startupNudgeUnsent            = "definitely_unsent"
	startupNudgePasting           = "paste_in_flight"
	startupNudgeTyped             = "typed_checkpoint"
	startupNudgeSubmitting        = "submit_in_flight"
	startupNudgeSubmitted         = "transport_submitted"
	startupNudgeUncertain         = "uncertain"
	startupNudgeConfirmed         = "app_confirmed"
	startupNudgeOperatorConfirmed = "operator_confirmed"
)

type startupNudgeIntent struct {
	SchemaVersion     int       `json:"schema_version"`
	ID                string    `json:"id"`
	SessionName       string    `json:"session_name"`
	Message           string    `json:"message"`
	MessageDigest     string    `json:"message_digest"`
	ProviderFamily    string    `json:"provider_family"`
	SessionID         string    `json:"session_id,omitempty"`
	InstanceToken     string    `json:"instance_token,omitempty"`
	RuntimeEpoch      string    `json:"runtime_epoch,omitempty"`
	ContinuationEpoch string    `json:"continuation_epoch,omitempty"`
	Target            string    `json:"target"`
	CreatedAt         time.Time `json:"created_at"`
}

type startupNudgeState struct {
	IntentID              string    `json:"intent_id"`
	Revision              uint64    `json:"revision"`
	Status                string    `json:"status"`
	Progress              string    `json:"progress,omitempty"`
	Reason                string    `json:"reason,omitempty"`
	PodName               string    `json:"pod_name,omitempty"`
	PodUID                string    `json:"pod_uid,omitempty"`
	ContainerID           string    `json:"container_id,omitempty"`
	ContainerRestartCount int32     `json:"container_restart_count,omitempty"`
	PanePID               string    `json:"pane_pid,omitempty"`
	Reported              bool      `json:"reported,omitempty"`
	OperatorAction        string    `json:"operator_action,omitempty"`
	OperatorIncarnation   string    `json:"operator_incarnation,omitempty"`
	OperatorAt            time.Time `json:"operator_at,omitempty"`
	UpdatedAt             time.Time `json:"updated_at"`
}

func (p *Provider) persistStartupNudgeIntent(ctx context.Context, name string, cfg runtime.Config) (*startupNudgeIntent, error) {
	message := cfg.Nudge
	if strings.TrimSpace(message) == "" {
		return nil, nil
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Env["GC_PROVIDER"]))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(cfg.ProviderName))
	}
	if provider == "" {
		return nil, errors.New("startup nudge provider family is missing")
	}
	sessionID := strings.TrimSpace(cfg.Env["GC_SESSION_ID"])
	instanceToken := strings.TrimSpace(cfg.Env["GC_INSTANCE_TOKEN"])
	if sessionID == "" {
		return nil, errors.New("startup nudge requires GC_SESSION_ID so recovery can acquire the owning runtime lease")
	}
	if instanceToken == "" {
		return nil, errors.New("startup nudge requires GC_INSTANCE_TOKEN to fence the runtime incarnation")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("startup nudge intent ID could not be generated")
	}
	messageSum := sha256.Sum256([]byte(message))
	intent := &startupNudgeIntent{
		SchemaVersion:     1,
		ID:                hex.EncodeToString(nonce),
		SessionName:       name,
		Message:           message,
		MessageDigest:     hex.EncodeToString(messageSum[:]),
		ProviderFamily:    provider,
		SessionID:         sessionID,
		InstanceToken:     instanceToken,
		RuntimeEpoch:      strings.TrimSpace(cfg.Env["GC_RUNTIME_EPOCH"]),
		ContinuationEpoch: strings.TrimSpace(cfg.Env["GC_CONTINUATION_EPOCH"]),
		Target:            tmuxSession,
		CreatedAt:         time.Now().UTC(),
	}
	intentKey := startupNudgeIntentKey(intent.ID)
	state := startupNudgeState{IntentID: intent.ID, Revision: 1, Status: startupNudgeUnsent, UpdatedAt: time.Now().UTC()}
	intentData, err := json.Marshal(intent)
	if err != nil {
		return nil, errors.New("startup nudge intent could not be encoded")
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		return nil, errors.New("startup nudge checkpoint could not be encoded")
	}
	err = p.withControllerMetadataLock(ctx, name, func(root string) error {
		// Make the initial checkpoint durable before the immutable intent file
		// becomes visible to recovery. If the process stops between these
		// writes, the orphan checkpoint is harmless and no launch has begun.
		statePath := controllerMetadataValuePath(root, name, startupNudgeStateKey(intent.ID))
		if err := writeControllerPrivateFile(statePath, stateData); err != nil {
			return err
		}
		intentPath := controllerMetadataValuePath(root, name, intentKey)
		file, err := os.OpenFile(intentPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := file.Write(intentData); err != nil {
			file.Close() //nolint:errcheck
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close() //nolint:errcheck
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return syncControllerMetadataDir(root)
	})
	if err != nil {
		return nil, fmt.Errorf("persisting immutable startup nudge intent failed")
	}
	return intent, nil
}

func startupNudgeIntentKey(id string) string { return "STARTUP_NUDGE_INTENT_" + strings.ToUpper(id) }
func startupNudgeStateKey(id string) string  { return "STARTUP_NUDGE_STATE_" + strings.ToUpper(id) }

func writeControllerPrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".startup-nudge-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

func (p *Provider) updateStartupNudgeState(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState) error {
	state.IntentID = intent.ID
	state.UpdatedAt = time.Now().UTC()
	if prior, err := p.loadStartupNudgeState(ctx, intent); err == nil {
		state.Revision = prior.Revision + 1
		if state.OperatorAction == "" {
			state.OperatorAction = prior.OperatorAction
		}
		if state.OperatorIncarnation == "" {
			state.OperatorIncarnation = prior.OperatorIncarnation
		}
		if state.OperatorAt.IsZero() {
			state.OperatorAt = prior.OperatorAt
		}
	} else if state.Revision == 0 {
		state.Revision = 1
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return p.setPrivateMetadata(ctx, intent.SessionName, startupNudgeStateKey(intent.ID), string(data))
}

// updateStartupNudgeStateBounded keeps the final uncertainty write possible
// when the transport context itself has expired. It is used only for
// checkpointing an already observed outcome; its fresh context is bounded.
func (p *Provider) updateStartupNudgeStateBounded(intent *startupNudgeIntent, state startupNudgeState) error {
	ctx, cancel := context.WithTimeout(context.Background(), k8sMetaOpTimeout)
	defer cancel()
	return p.updateStartupNudgeState(ctx, intent, state)
}

func (p *Provider) loadStartupNudgeState(ctx context.Context, intent *startupNudgeIntent) (startupNudgeState, error) {
	value, err := p.getPrivateMetadata(ctx, intent.SessionName, startupNudgeStateKey(intent.ID))
	if err != nil {
		return startupNudgeState{}, err
	}
	if value == "" {
		return startupNudgeState{IntentID: intent.ID, Status: startupNudgeUnsent}, nil
	}
	var state startupNudgeState
	if err := json.Unmarshal([]byte(value), &state); err != nil || state.IntentID != intent.ID {
		return startupNudgeState{}, errors.New("startup nudge checkpoint is invalid")
	}
	return state, nil
}

func (p *Provider) deliverStartupNudge(ctx context.Context, intent *startupNudgeIntent) error {
	if intent == nil {
		return nil
	}
	boundedCtx, cancel := context.WithTimeout(ctx, k8sMetaOpTimeout)
	defer cancel()
	ctx = boundedCtx
	podName, err := p.findRunningPod(ctx, intent.SessionName)
	if err != nil {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUnsent, Reason: "runtime_not_available"})
	}
	pod, err := p.ops.getPod(ctx, podName)
	if err != nil {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUnsent, Reason: "runtime_generation_unavailable"})
	}
	if strings.TrimSpace(string(pod.UID)) == "" {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "pod_generation_unavailable", PodName: podName})
	}
	containerID, restartCount, ok := agentContainerGeneration(pod)
	if !ok {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "container_generation_unavailable", PodName: podName, PodUID: string(pod.UID)})
	}
	if err := p.verifyStartupNudgeIncarnation(ctx, intent, podName); err != nil {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "session_incarnation_mismatch", PodName: podName, PodUID: string(pod.UID)})
	}
	panePID, err := p.execTmuxOwner(ctx, podName, []string{"tmux", "display-message", "-p", "-t", intent.Target, "#{pane_pid}"}, nil)
	if err != nil || strings.TrimSpace(panePID) == "" {
		return p.markStartupNudgeUncertain(ctx, intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "pane_generation_unavailable", PodName: podName, PodUID: string(pod.UID)})
	}
	state := startupNudgeState{
		Status: startupNudgePasting, Progress: "0/1", PodName: podName, PodUID: string(pod.UID),
		ContainerID: containerID, ContainerRestartCount: restartCount, PanePID: strings.TrimSpace(panePID),
	}
	if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
		return errors.New("startup nudge paste checkpoint could not be persisted")
	}
	if err := p.verifyStartupNudgePane(ctx, podName, intent.Target, state.PodUID, state.ContainerID, state.ContainerRestartCount, state.PanePID); err != nil {
		state.Status, state.Reason = startupNudgeUnsent, "pane_generation_changed_before_paste"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge target pane generation changed")
	}
	if _, err := p.execTmuxOwner(ctx, podName, append([]string{"tmux", "send-keys", "-t", intent.Target, "-l"}, runtime.TmuxLiteralArgs(intent.Message)...), nil); err != nil {
		state.Status, state.Reason = startupNudgeUncertain, "paste_result_ambiguous"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge paste result is uncertain")
	}
	state.Status, state.Progress = startupNudgeTyped, "1/1"
	if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
		state.Status, state.Reason = startupNudgeUncertain, "typed_checkpoint_persistence_failed"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge typed checkpoint could not be persisted")
	}
	return p.submitStartupNudge(ctx, intent, state, 0)
}

func (p *Provider) submitStartupNudge(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState, startAt int) error {
	return p.submitStartupNudgeWithDebounce(ctx, intent, state, startAt, true)
}

func (p *Provider) submitStartupNudgeWithDebounce(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState, startAt int, debounce bool) error {
	keys, ok := startupNudgeSubmitKeys(intent.ProviderFamily)
	if !ok || startAt < 0 || startAt > len(keys) {
		state.Status, state.Reason = startupNudgeUncertain, "provider_submit_layout_unsupported"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge provider family has no supported submit sequence")
	}
	if debounce && startAt == 0 {
		timer := time.NewTimer(startupNudgeDebounce)
		select {
		case <-ctx.Done():
			timer.Stop()
			state.Status, state.Reason = startupNudgeUncertain, "typed_draft_not_submitted"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge typed draft was not submitted")
		case <-timer.C:
		}
	}
	for i := startAt; i < len(keys); i++ {
		if err := p.verifyStartupNudgePane(ctx, state.PodName, intent.Target, state.PodUID, state.ContainerID, state.ContainerRestartCount, state.PanePID); err != nil {
			state.Status, state.Reason = startupNudgeUncertain, "pane_generation_changed_during_submit"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge target pane generation changed")
		}
		state.Status = startupNudgeSubmitting
		state.Progress = fmt.Sprintf("%d/%d", i, len(keys))
		if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
			state.Status, state.Reason = startupNudgeUncertain, "submit_checkpoint_persistence_failed"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge submit checkpoint could not be persisted")
		}
		if _, err := p.execTmuxOwner(ctx, state.PodName, []string{"tmux", "send-keys", "-t", intent.Target, keys[i]}, nil); err != nil {
			state.Status, state.Reason = startupNudgeUncertain, "submit_result_ambiguous"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge submit result is uncertain")
		}
		state.Progress = fmt.Sprintf("%d/%d", i+1, len(keys))
		if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
			state.Status, state.Reason = startupNudgeUncertain, "submit_progress_checkpoint_failed"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge submit progress could not be persisted")
		}
	}
	state.Status = startupNudgeSubmitted
	state.Progress = fmt.Sprintf("%d/%d", len(keys), len(keys))
	state.Reason = "transport_submitted; application consumption is not observable"
	if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
		return errors.New("startup nudge transport-submitted checkpoint could not be persisted")
	}
	return nil
}

type startupComposerGeometry struct {
	Width   int
	Height  int
	CursorX int
	CursorY int
}

// inspectStartupComposer accepts only the narrow, recognizable one-line
// composer layout whose visible draft and cursor can be established exactly.
// Wrapped, multiline, non-ASCII, modal, and unknown layouts remain uncertain.
func inspectStartupComposer(provider, message, screen string, geometry startupComposerGeometry) (bool, string) {
	if message == "" || strings.ContainsAny(message, "\r\n\t") || !isASCII(message) {
		return false, "composer_draft_requires_multiline_or_non_ascii_inspection"
	}
	if geometry.Width <= 0 || geometry.Height <= 0 || geometry.CursorX < 0 || geometry.CursorY < 0 || geometry.CursorX >= geometry.Width || geometry.CursorY >= geometry.Height {
		return false, "composer_geometry_unavailable"
	}
	prefixes := map[string][]string{
		"claude":    {"❯ "},
		"anthropic": {"❯ "},
		"codex":     {"› "},
		"openai":    {"› "},
	}
	knownPrefixes, ok := prefixes[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		return false, "composer_provider_layout_unsupported"
	}
	lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
	if geometry.CursorY >= len(lines) {
		return false, "composer_cursor_outside_captured_screen"
	}
	line := strings.TrimRight(lines[geometry.CursorY], "\r")
	if geometry.CursorY < (geometry.Height+1)/2 {
		return false, "composer_cursor_outside_bottom_prompt_region"
	}
	for _, prefix := range knownPrefixes {
		want := prefix + message
		// The visible cursor must sit immediately after the exact draft. This
		// excludes transcript echoes and a prompt with an appended suffix.
		if line == want && geometry.CursorX == len([]rune(want)) && geometry.CursorX < geometry.Width-1 {
			return true, ""
		}
	}
	return false, "composer_draft_or_cursor_does_not_match_exact_intent"
}

func hasStartupComposerModal(screen string) bool {
	lower := strings.ToLower(screen)
	for _, marker := range []string{
		"do you want to", "allow once", "allow for this session", "permission request",
		"approval required", "press enter to continue", "[y/n]",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func (p *Provider) inspectStartupComposer(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState) (bool, string) {
	geometryOutput, err := p.execTmuxOwner(ctx, state.PodName, []string{
		"tmux", "display-message", "-p", "-t", intent.Target,
		"#{pane_width} #{pane_height} #{cursor_x} #{cursor_y}",
	}, nil)
	if err != nil {
		return false, "composer_geometry_unavailable"
	}
	var geometry startupComposerGeometry
	if _, err := fmt.Sscanf(strings.TrimSpace(geometryOutput), "%d %d %d %d", &geometry.Width, &geometry.Height, &geometry.CursorX, &geometry.CursorY); err != nil {
		return false, "composer_geometry_unavailable"
	}
	screen, err := p.execTmuxOwner(ctx, state.PodName, []string{
		"tmux", "capture-pane", "-p", "-N", "-t", intent.Target,
	}, nil)
	if err != nil {
		return false, "composer_screen_unavailable"
	}
	if hasStartupComposerModal(screen) {
		return false, "composer_modal_or_confirmation_detected"
	}
	return inspectStartupComposer(intent.ProviderFamily, intent.Message, screen, geometry)
}

func startupNudgeSubmitProgress(state startupNudgeState, keyCount int) (int, bool) {
	parts := strings.Split(state.Progress, "/")
	if len(parts) != 2 {
		return 0, false
	}
	var completed, total int
	if _, err := fmt.Sscanf(state.Progress, "%d/%d", &completed, &total); err != nil || total != keyCount || completed < 0 || completed > total {
		return 0, false
	}
	return completed, true
}

func resumeStartupNudgeSubmission(ctx context.Context, p *Provider, intent *startupNudgeIntent, state startupNudgeState) error {
	if err := p.verifyStartupNudgeIncarnation(ctx, intent, state.PodName); err != nil {
		state.Status, state.Reason = startupNudgeUncertain, "session_incarnation_mismatch"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge target incarnation changed")
	}
	if err := p.verifyStartupNudgePane(ctx, state.PodName, intent.Target, state.PodUID, state.ContainerID, state.ContainerRestartCount, state.PanePID); err != nil {
		state.Status, state.Reason = startupNudgeUncertain, "pane_generation_changed_before_resume"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge target pane generation changed")
	}
	exact, reason := p.inspectStartupComposer(ctx, intent, state)
	if !exact {
		state.Status, state.Reason = startupNudgeUncertain, reason
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge composer could not be established exactly")
	}
	keys, ok := startupNudgeSubmitKeys(intent.ProviderFamily)
	if !ok {
		state.Status, state.Reason = startupNudgeUncertain, "composer_provider_layout_unsupported"
		_ = p.updateStartupNudgeStateBounded(intent, state)
		return errors.New("startup nudge provider submit sequence is unsupported")
	}
	startAt := 0
	if state.Status == startupNudgeSubmitting {
		completed, valid := startupNudgeSubmitProgress(state, len(keys))
		if !valid {
			state.Status, state.Reason = startupNudgeUncertain, "submit_progress_checkpoint_invalid"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge submit progress is invalid")
		}
		startAt = completed
	} else {
		state.Status, state.Progress = startupNudgeTyped, "1/1"
		if err := p.updateStartupNudgeState(ctx, intent, state); err != nil {
			state.Status, state.Reason = startupNudgeUncertain, "typed_checkpoint_persistence_failed"
			_ = p.updateStartupNudgeStateBounded(intent, state)
			return errors.New("startup nudge typed checkpoint could not be persisted during recovery")
		}
	}
	// The original debounce has already elapsed before this checkpoint.
	// Avoid applying it again after a controller restart.
	return p.submitStartupNudgeWithDebounce(ctx, intent, state, startAt, false)
}

func startupNudgeSubmitKeys(provider string) ([]string, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "anthropic":
		return []string{"Enter"}, true
	case "codex", "openai":
		return []string{"Escape", "Enter"}, true
	default:
		return nil, false
	}
}

func (p *Provider) verifyStartupNudgeIncarnation(ctx context.Context, intent *startupNudgeIntent, podName string) error {
	if intent.SessionID == "" && intent.InstanceToken == "" {
		return errors.New("startup nudge has no session incarnation identity")
	}
	for key, want := range map[string]string{
		"GC_SESSION_ID":         intent.SessionID,
		"GC_INSTANCE_TOKEN":     intent.InstanceToken,
		"GC_RUNTIME_EPOCH":      intent.RuntimeEpoch,
		"GC_CONTINUATION_EPOCH": intent.ContinuationEpoch,
	} {
		if want == "" {
			continue
		}
		got, err := p.getMetaFromPod(ctx, podName, intent.SessionName, key)
		if err != nil || got != want {
			return errors.New("startup nudge target incarnation changed")
		}
	}
	provider, err := p.getMetaFromPod(ctx, podName, intent.SessionName, "GC_PROVIDER")
	if err != nil || strings.ToLower(strings.TrimSpace(provider)) != intent.ProviderFamily {
		return errors.New("startup nudge provider family changed")
	}
	return nil
}

func (p *Provider) verifyStartupNudgePane(ctx context.Context, podName, target, expectedUID, expectedContainerID string, expectedContainerRestart int32, expectedPID string) error {
	pod, err := p.ops.getPod(ctx, podName)
	if err != nil || string(pod.UID) != expectedUID {
		return errors.New("startup nudge pod generation changed")
	}
	containerID, restartCount, ok := agentContainerGeneration(pod)
	if !ok || containerID != expectedContainerID || restartCount != expectedContainerRestart {
		return errors.New("startup nudge container generation changed")
	}
	panePID, err := p.execTmuxOwner(ctx, podName, []string{"tmux", "display-message", "-p", "-t", target, "#{pane_pid}"}, nil)
	if err != nil || strings.TrimSpace(panePID) != expectedPID {
		return errors.New("startup nudge pane generation changed")
	}
	return nil
}

func agentContainerGeneration(pod *corev1.Pod) (string, int32, bool) {
	if pod == nil {
		return "", 0, false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "agent" || status.State.Running == nil || strings.TrimSpace(status.ContainerID) == "" {
			continue
		}
		return status.ContainerID, status.RestartCount, true
	}
	return "", 0, false
}

func (p *Provider) markStartupNudgeUncertain(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState) error {
	if state.Status == "" {
		state.Status = startupNudgeUncertain
	}
	if state.Status == startupNudgeUnsent {
		state.Reason = "definitely_unsent; runtime is not currently available"
	}
	var err error
	if ctx.Err() != nil {
		err = p.updateStartupNudgeStateBounded(intent, state)
	} else {
		err = p.updateStartupNudgeState(ctx, intent, state)
	}
	if err != nil {
		return errors.New("startup nudge unresolved state could not be persisted")
	}
	return errors.New("startup nudge delivery remains unresolved")
}

func (p *Provider) startupNudgeDegradedError(ctx context.Context, intent *startupNudgeIntent) error {
	state, err := p.loadStartupNudgeState(ctx, intent)
	if err != nil {
		state = startupNudgeState{Status: startupNudgeUncertain, Reason: "startup nudge checkpoint is unavailable"}
	}
	if state.Status == "" {
		state.Status = startupNudgeUncertain
	}
	return &runtime.StartupNudgeDegradedError{IntentID: intent.ID, Status: state.Status, Reason: state.Reason}
}

// RecoverStartupNudges scans controller-owned immutable intents after startup
// and during periodic controller passes. It retries only definitely-unsent
// intents against the same incarnation. A typed draft or ambiguous submit is
// surfaced as uncertain and never replayed based on transcript text.
func (p *Provider) RecoverStartupNudges(ctx context.Context) []runtime.StartupNudgeStatus {
	return p.recoverStartupNudges(ctx, "", "")
}

// StartupNudgeSessionNames returns the session names with outstanding durable
// startup delivery intents owned by this provider.
func (p *Provider) StartupNudgeSessionNames(ctx context.Context) []string {
	targets := p.StartupNudgeLeaseTargets(ctx)
	ordered := make([]string, 0, len(targets))
	for _, target := range targets {
		ordered = append(ordered, target.SessionName)
	}
	return ordered
}

// StartupNudgeLeaseTargets returns the newest pending intent for each session
// so the controller can acquire the owning session row's runtime lease.
func (p *Provider) StartupNudgeLeaseTargets(ctx context.Context) []runtime.StartupNudgeLeaseTarget {
	if ctx == nil || ctx.Err() != nil {
		return nil
	}
	root, err := controllerMetadataRoot(p.cityPath)
	if err != nil {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(root, "*-STARTUP_NUDGE_INTENT_*"))
	if err != nil {
		return nil
	}
	targets := make(map[string]runtime.StartupNudgeLeaseTarget)
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		data, err := os.ReadFile(path)
		var intent startupNudgeIntent
		if err == nil && json.Unmarshal(data, &intent) == nil && validStartupNudgeIntent(intent) {
			if !startupNudgeNeedsRecovery(root, intent) {
				continue
			}
			target := runtime.StartupNudgeLeaseTarget{SessionName: intent.SessionName, SessionID: intent.SessionID, CreatedAt: intent.CreatedAt}
			prior, exists := targets[intent.SessionName]
			if !exists || target.CreatedAt.After(prior.CreatedAt) || (target.CreatedAt.Equal(prior.CreatedAt) && target.SessionID < prior.SessionID) {
				targets[intent.SessionName] = target
			}
		}
	}
	ordered := make([]runtime.StartupNudgeLeaseTarget, 0, len(targets))
	for _, target := range targets {
		ordered = append(ordered, target)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SessionName < ordered[j].SessionName })
	return ordered
}

func startupNudgeNeedsRecovery(root string, intent startupNudgeIntent) bool {
	statePath := controllerMetadataValuePath(root, intent.SessionName, startupNudgeStateKey(intent.ID))
	data, err := os.ReadFile(statePath)
	if err != nil {
		return true
	}
	var state startupNudgeState
	if json.Unmarshal(data, &state) != nil || state.IntentID != intent.ID {
		return true
	}
	if !state.Reported {
		return true
	}
	switch state.Status {
	case startupNudgeUncertain, startupNudgeSubmitted, startupNudgeConfirmed, startupNudgeOperatorConfirmed:
		return false
	default:
		return true
	}
}

// RecoverStartupNudgesForSession recovers pending startup delivery intents for
// one session after its current route has been resolved.
func (p *Provider) RecoverStartupNudgesForSession(ctx context.Context, sessionName string) []runtime.StartupNudgeStatus {
	return p.recoverStartupNudges(ctx, sessionName, "")
}

// RecoverStartupNudgesForLeasedSession recovers pending startup delivery
// intents only when they match the session row held by the controller lease.
func (p *Provider) RecoverStartupNudgesForLeasedSession(ctx context.Context, sessionName, sessionID string) []runtime.StartupNudgeStatus {
	return p.recoverStartupNudges(ctx, sessionName, sessionID)
}

// AcknowledgeStartupNudgeEvent closes the report-pending checkpoint only
// after the controller has appended the matching durable event.
func (p *Provider) AcknowledgeStartupNudgeEvent(ctx context.Context, sessionName, intentID string, revision uint64) error {
	if ctx == nil || strings.TrimSpace(sessionName) == "" || len(intentID) != 32 || revision == 0 {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	if _, err := hex.DecodeString(intentID); err != nil {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := p.acquireControllerGenerationLock(ctx, sessionName)
	if err != nil {
		return fmt.Errorf("startup nudge event acknowledgement generation fence unavailable")
	}
	defer release()
	root, err := controllerMetadataRoot(p.cityPath)
	if err != nil {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	path := controllerMetadataValuePath(root, sessionName, startupNudgeIntentKey(intentID))
	intent, err := readStartupNudgeIntent(path)
	if err != nil || !validStartupNudgeIntent(intent) || intent.SessionName != sessionName || intent.ID != intentID {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	state, err := p.loadStartupNudgeState(ctx, &intent)
	if err != nil {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	if state.Reported {
		return nil
	}
	if state.Revision != revision {
		return runtime.ErrStartupNudgeCheckpointUnavailable
	}
	state.Reported = true
	return p.updateStartupNudgeState(ctx, &intent, state)
}

func (p *Provider) recoverStartupNudges(ctx context.Context, sessionName, expectedSessionID string) []runtime.StartupNudgeStatus {
	if ctx == nil {
		ctx = context.Background()
	}
	root, err := controllerMetadataRoot(p.cityPath)
	if err != nil {
		return []runtime.StartupNudgeStatus{{Status: "recovery_error", Reason: "controller metadata directory unavailable"}}
	}
	paths, err := filepath.Glob(filepath.Join(root, "*-STARTUP_NUDGE_INTENT_*"))
	if err != nil {
		return []runtime.StartupNudgeStatus{{Status: "recovery_error", Reason: "startup nudge intent scan failed"}}
	}
	sort.Strings(paths)
	var reports []runtime.StartupNudgeStatus
	for _, path := range paths {
		if ctx.Err() != nil {
			return reports
		}
		data, readErr := os.ReadFile(path)
		var intent startupNudgeIntent
		if readErr != nil || json.Unmarshal(data, &intent) != nil || !validStartupNudgeIntent(intent) {
			reports = append(reports, runtime.StartupNudgeStatus{Status: "recovery_error", Reason: "startup nudge intent is unreadable"})
			continue
		}
		if sessionName != "" && intent.SessionName != sessionName {
			continue
		}
		intentCtx, cancel := context.WithTimeout(ctx, k8sMetaOpTimeout)
		release, lockErr := p.acquireControllerGenerationLock(intentCtx, intent.SessionName)
		if lockErr != nil {
			cancel()
			reports = append(reports, nudgeReport(intent, startupNudgeState{Status: "recovery_error", Reason: "session generation fence unavailable"}, false))
			continue
		}
		state, stateErr := p.loadStartupNudgeState(intentCtx, &intent)
		if stateErr != nil {
			state = startupNudgeState{Status: startupNudgeUncertain, Reason: "startup nudge checkpoint is unreadable"}
			changed := false
			if err := p.updateStartupNudgeState(intentCtx, &intent, state); err == nil {
				changed = true
			} else {
				state.Reason = "startup nudge checkpoint is unreadable and uncertainty could not be persisted"
			}
			state = p.reportableStartupNudgeState(intentCtx, &intent, state, &changed)
			release()
			cancel()
			reports = append(reports, nudgeReport(intent, state, changed))
			continue
		}
		if expectedSessionID != "" && intent.SessionID != expectedSessionID {
			reason := "session_row_incarnation_mismatch"
			changed := state.Status != startupNudgeUncertain || state.Reason != reason || !state.Reported
			state.Status, state.Reason, state.Reported = startupNudgeUncertain, reason, false
			if err := p.updateStartupNudgeState(intentCtx, &intent, state); err != nil {
				state.Status, state.Reason, changed = startupNudgeUncertain, "session row incarnation mismatch; uncertainty could not be persisted", false
			}
			state = p.reportableStartupNudgeState(intentCtx, &intent, state, &changed)
			release()
			cancel()
			reports = append(reports, nudgeReport(intent, state, changed))
			continue
		}
		changed := false
		switch state.Status {
		case startupNudgeUnsent:
			err := p.deliverStartupNudge(intentCtx, &intent)
			state, stateErr = p.loadStartupNudgeState(intentCtx, &intent)
			if stateErr == nil && (state.Status == startupNudgeUncertain || state.Status == startupNudgeSubmitted) {
				changed = true
			}
			if err != nil && stateErr != nil {
				state = startupNudgeState{Status: "recovery_error", Reason: "startup nudge delivery failed without a readable checkpoint"}
			}
		case startupNudgePasting, startupNudgeTyped, startupNudgeSubmitting:
			priorRevision := state.Revision
			resumeErr := resumeStartupNudgeSubmission(intentCtx, p, &intent, state)
			state, stateErr = p.loadStartupNudgeState(intentCtx, &intent)
			if stateErr != nil {
				state = startupNudgeState{Status: startupNudgeUncertain, Reason: "startup nudge recovery checkpoint is unreadable"}
			} else if state.Revision != priorRevision {
				// A checkpoint written during recovery is a durable state
				// transition that must reach the controller event stream. The
				// event acknowledgement happens only after that append succeeds.
				changed = true
			}
			if resumeErr != nil && state.Status != startupNudgeUncertain {
				state.Status, state.Reason = startupNudgeUncertain, "startup nudge composer could not be safely resumed"
				stateErr = errors.New("recovery outcome changed")
			}
			if stateErr != nil {
				if err := p.updateStartupNudgeState(intentCtx, &intent, state); err != nil {
					state.Status, state.Reason = startupNudgeUncertain, "startup nudge recovery result could not be durably recorded"
					changed = false
				} else {
					changed = true
				}
			}
		case startupNudgeUncertain:
			// Durable uncertainty remains visible; never re-send without an
			// explicit operator decision bound to this intent and incarnation.
			if !state.Reported {
				changed = true
			}
		case startupNudgeSubmitted, startupNudgeConfirmed:
			// Transport submission is terminal for automatic delivery. Record
			// the terminal transition once; the application does not expose a
			// reliable consumption receipt.
			if !state.Reported {
				changed = true
			}
		case startupNudgeOperatorConfirmed:
			if !state.Reported {
				changed = true
			}
		default:
			state.Status = startupNudgeUncertain
			state.Reason = "unknown startup nudge state"
			if err := p.updateStartupNudgeState(intentCtx, &intent, state); err == nil {
				changed = true
			}
		}
		state = p.reportableStartupNudgeState(intentCtx, &intent, state, &changed)
		release()
		cancel()
		if changed || state.Status == startupNudgeUncertain || state.Status == startupNudgeOperatorConfirmed {
			reports = append(reports, nudgeReport(intent, state, changed))
		}
	}
	return reports
}

// reportableStartupNudgeState reloads the durable revision used to fence the
// later event acknowledgement. A failed checkpoint write is never reported as
// a durable transition.
func (p *Provider) reportableStartupNudgeState(ctx context.Context, intent *startupNudgeIntent, state startupNudgeState, changed *bool) startupNudgeState {
	latest, err := p.loadStartupNudgeState(ctx, intent)
	if err != nil || latest.Status != state.Status || latest.Reason != state.Reason {
		*changed = false
		return state
	}
	return latest
}

func nudgeReport(intent startupNudgeIntent, state startupNudgeState, changed bool) runtime.StartupNudgeStatus {
	incarnation := intent.InstanceToken
	if incarnation == "" {
		incarnation = strings.Join([]string{intent.SessionID, intent.RuntimeEpoch, intent.ContinuationEpoch}, "/")
	}
	return runtime.StartupNudgeStatus{
		SessionName: intent.SessionName, IntentID: intent.ID,
		ProviderFamily: intent.ProviderFamily, Incarnation: incarnation,
		Status: state.Status, Reason: state.Reason, CheckpointRevision: state.Revision, Changed: changed,
	}
}

// StartupNudgeDiagnostics returns unresolved controller-owned startup intents
// without reading their prompt text or changing their delivery state.
func StartupNudgeDiagnostics(cityPath string) ([]runtime.StartupNudgeStatus, error) {
	root := filepath.Join(strings.TrimSpace(cityPath), ".gc", "runtime-meta", "k8s")
	if strings.TrimSpace(cityPath) == "" {
		return nil, errors.New("city path is required")
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*-STARTUP_NUDGE_INTENT_*"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var diagnostics []runtime.StartupNudgeStatus
	for _, path := range paths {
		data, err := os.ReadFile(path)
		var intent startupNudgeIntent
		if err != nil || json.Unmarshal(data, &intent) != nil || !validStartupNudgeIntent(intent) {
			diagnostics = append(diagnostics, runtime.StartupNudgeStatus{Status: "recovery_error", Reason: "startup nudge intent is unreadable"})
			continue
		}
		statePath := filepath.Join(root, strings.TrimSuffix(filepath.Base(path), startupNudgeIntentKey(intent.ID))+startupNudgeStateKey(intent.ID))
		stateData, err := os.ReadFile(statePath)
		if err != nil {
			diagnostics = append(diagnostics, nudgeReport(intent, startupNudgeState{Status: startupNudgeUncertain, Reason: "startup nudge checkpoint is unavailable"}, false))
			continue
		}
		var state startupNudgeState
		if json.Unmarshal(stateData, &state) != nil || state.IntentID != intent.ID {
			state = startupNudgeState{Status: startupNudgeUncertain, Reason: "startup nudge checkpoint is unreadable"}
		}
		if state.Status != startupNudgeSubmitted && state.Status != startupNudgeConfirmed && state.Status != startupNudgeOperatorConfirmed {
			diagnostics = append(diagnostics, nudgeReport(intent, state, false))
		}
	}
	return diagnostics, nil
}

var (
	_ runtime.StartupNudgeRecoveryProvider      = (*Provider)(nil)
	_ runtime.StartupNudgeSessionProvider       = (*Provider)(nil)
	_ runtime.StartupNudgeLeasedSessionProvider = (*Provider)(nil)
	_ runtime.StartupNudgeLeaseTargetProvider   = (*Provider)(nil)
	_ runtime.StartupNudgeEventAcknowledger     = (*Provider)(nil)
)

func validStartupNudgeIntent(intent startupNudgeIntent) bool {
	if intent.SchemaVersion != 1 || intent.ID == "" || intent.SessionName == "" || intent.Target != tmuxSession {
		return false
	}
	if _, err := hex.DecodeString(intent.ID); err != nil || len(intent.ID) != 32 {
		return false
	}
	sum := sha256.Sum256([]byte(intent.Message))
	return hex.EncodeToString(sum[:]) == intent.MessageDigest && strings.TrimSpace(intent.ProviderFamily) != ""
}
