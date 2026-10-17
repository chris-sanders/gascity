package runtime

import (
	"context"
	"sort"
	"time"
)

// StartupNudgeStatus is a durable delivery observation reported by a runtime
// that owns startup prompt delivery. Transport submission does not imply that
// the application consumed the prompt.
type StartupNudgeStatus struct {
	SessionName    string `json:"session_name"`
	IntentID       string `json:"intent_id"`
	ProviderFamily string `json:"provider_family"`
	Incarnation    string `json:"incarnation"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	Changed        bool   `json:"changed,omitempty"`
}

// StartupNudgeRecoveryProvider scans controller-owned delivery intents for
// the provider's sessions. Implementations must never replay an operation
// whose prior transport result is ambiguous.
type StartupNudgeRecoveryProvider interface {
	RecoverStartupNudges(context.Context) []StartupNudgeStatus
}

// StartupNudgeSessionProvider exposes the durable session names owned by a
// backend and recovers one session at a time. The controller resolves the
// current route before recovery so a stale K8s intent cannot be delivered to
// a backend that no longer owns that session.
type StartupNudgeSessionProvider interface {
	StartupNudgeSessionNames(context.Context) []string
	RecoverStartupNudgesForSession(context.Context, string) []StartupNudgeStatus
}

// StartupNudgeLeasedSessionProvider can bind recovery to the session row held
// by the controller's runtime lease. Implementations reject intents from an
// older session incarnation before inspecting or sending pane input.
type StartupNudgeLeasedSessionProvider interface {
	RecoverStartupNudgesForLeasedSession(context.Context, string, string) []StartupNudgeStatus
}

type StartupNudgeLeaseTarget struct {
	SessionName string
	SessionID   string
	CreatedAt   time.Time
}

// StartupNudgeLeaseTargetProvider returns the newest immutable intent for
// each session name so the controller can acquire that exact session row's
// runtime lease before route resolution or recovery.
type StartupNudgeLeaseTargetProvider interface {
	StartupNudgeLeaseTargets(context.Context) []StartupNudgeLeaseTarget
}

// RecoverStartupNudges walks composite providers to their leaves and asks
// each startup-nudge owner to recover its outstanding durable intents. The
// backend listing is structural; each leaf filters intents by its own
// provider family and generation fences them before interacting with a pane.
func RecoverStartupNudges(ctx context.Context, provider Provider) []StartupNudgeStatus {
	if ctx == nil || ctx.Err() != nil || provider == nil {
		return nil
	}
	ordered := StartupNudgeSessionNames(ctx, provider)
	var statuses []StartupNudgeStatus
	for _, name := range ordered {
		if ctx.Err() != nil {
			return statuses
		}
		statuses = append(statuses, RecoverStartupNudgeForSession(ctx, provider, name)...)
	}
	if len(ordered) != 0 {
		return statuses
	}
	if owner, ok := provider.(StartupNudgeRecoveryProvider); ok {
		return owner.RecoverStartupNudges(ctx)
	}
	return nil
}

// StartupNudgeSessionNames returns the unique sessions with controller-owned
// startup intents on any leaf backend. It lets the controller hold each
// session's runtime lease while recovering that session's intent.
func StartupNudgeSessionNames(ctx context.Context, provider Provider) []string {
	if ctx == nil || ctx.Err() != nil || provider == nil {
		return nil
	}
	var leaves []Provider
	collectStartupNudgeLeaves(provider, &leaves)
	names := make(map[string]struct{})
	for _, leaf := range leaves {
		owner, ok := leaf.(StartupNudgeSessionProvider)
		if !ok {
			continue
		}
		for _, name := range owner.StartupNudgeSessionNames(ctx) {
			if name != "" {
				names[name] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	return ordered
}

// StartupNudgeLeaseTargets returns the newest pending-intent incarnation per
// session name from runtime leaves. Targets without an ID are retained so the
// controller can expose a fail-closed lease deferral.
func StartupNudgeLeaseTargets(ctx context.Context, provider Provider) []StartupNudgeLeaseTarget {
	if ctx == nil || ctx.Err() != nil || provider == nil {
		return nil
	}
	var leaves []Provider
	collectStartupNudgeLeaves(provider, &leaves)
	byName := make(map[string]StartupNudgeLeaseTarget)
	for _, leaf := range leaves {
		targetProvider, ok := leaf.(StartupNudgeLeaseTargetProvider)
		if !ok {
			continue
		}
		for _, target := range targetProvider.StartupNudgeLeaseTargets(ctx) {
			if target.SessionName == "" {
				continue
			}
			prior, exists := byName[target.SessionName]
			if !exists || target.CreatedAt.After(prior.CreatedAt) || (target.CreatedAt.Equal(prior.CreatedAt) && target.SessionID < prior.SessionID) {
				byName[target.SessionName] = target
			}
		}
	}
	ordered := make([]StartupNudgeLeaseTarget, 0, len(byName))
	for _, target := range byName {
		ordered = append(ordered, target)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SessionName < ordered[j].SessionName })
	return ordered
}

// RecoverStartupNudgeForSession resolves the current route for one session
// and recovers only that runtime owner's durable intent.
func RecoverStartupNudgeForSession(ctx context.Context, provider Provider, name string, sessionIDs ...string) []StartupNudgeStatus {
	if ctx == nil || ctx.Err() != nil || provider == nil || name == "" {
		return nil
	}
	leaf, _, known := ResolveBackend(provider, name)
	if !known {
		return []StartupNudgeStatus{{
			SessionName: name, Status: "uncertain", Reason: "runtime_route_unknown; startup nudge was not delivered",
		}}
	}
	owner, ok := leaf.(StartupNudgeSessionProvider)
	if !ok {
		return nil
	}
	if len(sessionIDs) > 0 && sessionIDs[0] != "" {
		if leased, ok := leaf.(StartupNudgeLeasedSessionProvider); ok {
			return leased.RecoverStartupNudgesForLeasedSession(ctx, name, sessionIDs[0])
		}
	}
	return owner.RecoverStartupNudgesForSession(ctx, name)
}

func collectStartupNudgeLeaves(provider Provider, leaves *[]Provider) {
	if composite, ok := provider.(BackendsProvider); ok {
		for _, backend := range composite.Backends() {
			if backend.Provider != nil {
				collectStartupNudgeLeaves(backend.Provider, leaves)
			}
		}
		return
	}
	*leaves = append(*leaves, provider)
}
