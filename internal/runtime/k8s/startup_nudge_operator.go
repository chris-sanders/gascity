package k8s

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResolveStartupNudge records an operator decision against one immutable
// intent and its exact session incarnation. A retry is authorized only after
// the operator has established that the prior prompt was not consumed.
func ResolveStartupNudge(cityPath, intentID, incarnation, decision string) (string, error) {
	intentID = strings.ToLower(strings.TrimSpace(intentID))
	incarnation = strings.TrimSpace(incarnation)
	decision = strings.TrimSpace(strings.ToLower(decision))
	if len(intentID) != 32 {
		return "", errors.New("startup nudge intent ID must be a 128-bit hexadecimal value")
	}
	if _, err := hex.DecodeString(intentID); err != nil {
		return "", errors.New("startup nudge intent ID must be a 128-bit hexadecimal value")
	}
	if incarnation == "" {
		return "", errors.New("startup nudge incarnation is required")
	}
	if decision != "delivered" && decision != "retry" {
		return "", errors.New("decision must be delivered or retry")
	}
	if strings.TrimSpace(cityPath) == "" {
		return "", errors.New("city path is required")
	}

	root, err := controllerMetadataRoot(cityPath)
	if err != nil {
		return "", errors.New("controller-owned startup nudge state is unavailable")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*-"+startupNudgeIntentKey(intentID)))
	if err != nil || len(paths) != 1 {
		return "", errors.New("startup nudge intent was not found uniquely")
	}
	data, err := readStartupNudgeIntent(paths[0])
	if err != nil || !validStartupNudgeIntent(data) {
		return "", errors.New("startup nudge intent is invalid")
	}
	if nudgeReport(data, startupNudgeState{}, false).Incarnation != incarnation {
		return "", errors.New("startup nudge incarnation does not match the immutable intent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), k8sMetaOpTimeout)
	defer cancel()
	p := &Provider{cityPath: cityPath}
	release, err := p.acquireControllerGenerationLock(ctx, data.SessionName)
	if err != nil {
		return "", errors.New("session generation fence is unavailable")
	}
	defer release()
	state, err := p.loadStartupNudgeState(ctx, &data)
	if err != nil {
		return "", errors.New("startup nudge checkpoint is unavailable")
	}
	switch decision {
	case "delivered":
		state.Status = startupNudgeOperatorConfirmed
		state.Reason = "operator confirmed the exact intent was consumed"
		state.Reported = false
		state.OperatorAction = "delivered"
	case "retry":
		state.Status = startupNudgeUnsent
		state.Reason = "operator confirmed the prior attempt was not consumed; explicit retry authorized"
		state.Progress = ""
		state.PodName, state.PodUID, state.PanePID = "", "", ""
		state.Reported = false
		state.OperatorAction = "retry"
	}
	state.OperatorIncarnation = incarnation
	state.OperatorAt = time.Now().UTC()
	if err := p.updateStartupNudgeState(ctx, &data, state); err != nil {
		return "", errors.New("operator resolution could not be persisted")
	}
	return state.Status, nil
}

func readStartupNudgeIntent(path string) (startupNudgeIntent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return startupNudgeIntent{}, err
	}
	var intent startupNudgeIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return startupNudgeIntent{}, err
	}
	return intent, nil
}
