package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

const startupNudgeControllerScanTimeout = 30 * time.Second

const (
	startupNudgeUncertainEvent = "session.startup_nudge_uncertain"
	startupNudgeResolvedEvent  = "session.startup_nudge_resolved"
	startupNudgeSubmittedEvent = "session.startup_nudge_transport_submitted"
)

// recoverStartupNudges is called during controller startup and on every
// reconciliation pass. Runtime owners persist intent and checkpoints; this
// layer makes newly discovered uncertainty visible in the city event stream.
func (cr *CityRuntime) recoverStartupNudges(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	scanCtx, cancel := context.WithTimeout(ctx, startupNudgeControllerScanTimeout)
	defer cancel()
	targets := runtime.StartupNudgeLeaseTargets(scanCtx, cr.sp)
	if len(targets) == 0 {
		cr.recordStartupNudgeRecovery(runtime.RecoverStartupNudges(scanCtx, cr.sp))
		return
	}
	sessionStore := cr.sessionsBeadStore().Store
	front := sessionFrontDoor(sessionStore)
	var statuses []runtime.StartupNudgeStatus
	for _, target := range targets {
		if scanCtx.Err() != nil {
			break
		}
		if target.SessionID == "" || front == nil {
			fmt.Fprintf(cr.stderr, "%s: startup nudge recovery for %s deferred: immutable intent has no session row ID\n", cr.logPrefix, target.SessionName) //nolint:errcheck
			continue
		}
		request := sessionpkg.RuntimeLeaseRequest{
			City: cr.cityPath, Name: target.SessionName, ID: target.SessionID,
			TTL: sessionpkg.RuntimeLeaseTTL(startupNudgeControllerScanTimeout),
		}
		lease, leaseErr := sessionpkg.TryRuntimeLease(front, request)
		if leaseErr != nil {
			fmt.Fprintf(cr.stderr, "%s: startup nudge recovery for %s deferred: runtime lease unavailable\n", cr.logPrefix, target.SessionName) //nolint:errcheck
			continue
		}
		leaseCtx, cancelLease, watchErr := lease.Watch(scanCtx, 5*time.Second)
		if watchErr != nil {
			lease.Release()
			fmt.Fprintf(cr.stderr, "%s: startup nudge recovery for %s deferred: runtime lease watch unavailable\n", cr.logPrefix, target.SessionName) //nolint:errcheck
			continue
		}
		statuses = append(statuses, runtime.RecoverStartupNudgeForSession(leaseCtx, cr.sp, target.SessionName, target.SessionID)...)
		cancelLease()
		lease.Release()
	}
	cr.recordStartupNudgeRecovery(statuses)
}

func (cr *CityRuntime) recordStartupNudgeRecovery(statuses []runtime.StartupNudgeStatus) {
	for _, status := range statuses {
		if !status.Changed {
			continue
		}
		eventType := ""
		switch status.Status {
		case "uncertain":
			eventType = startupNudgeUncertainEvent
		case "operator_confirmed":
			eventType = startupNudgeResolvedEvent
		case "transport_submitted":
			eventType = startupNudgeSubmittedEvent
		default:
			continue
		}
		payload, err := json.Marshal(status)
		if err != nil {
			fmt.Fprintf(cr.stderr, "%s: startup nudge %s for %s is uncertain\n", cr.logPrefix, status.IntentID, status.SessionName) //nolint:errcheck
			continue
		}
		if cr.rec != nil {
			cr.rec.Record(events.Event{
				Type:    eventType,
				Actor:   "gc",
				Subject: status.SessionName,
				Message: status.Reason,
				Payload: payload,
			})
		}
		fmt.Fprintf(cr.stderr, "%s: startup nudge %s for %s status=%s: %s\n", cr.logPrefix, status.IntentID, status.SessionName, status.Status, status.Reason) //nolint:errcheck
	}
}
