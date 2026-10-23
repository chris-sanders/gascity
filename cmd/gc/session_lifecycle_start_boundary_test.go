package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type blockingStartProvider struct {
	runtime.Provider
	started chan struct{}
	release chan struct{}
}

func (p *blockingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	close(p.started)
	select {
	case <-p.release:
		return p.Provider.Start(ctx, name, cfg)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type startUnavailableLivenessProvider struct {
	*runtime.Fake
}

func (p *startUnavailableLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.ObserveLiveness(p.Fake, name, processNames), fmt.Errorf("start preflight: %w", runtime.ErrRuntimeUnavailable)
}

type startConfirmedAbsentProvider struct {
	*runtime.Fake
}

func (p *startConfirmedAbsentProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

func TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Transport: "", Resume: sessionpkg.ProviderResume{}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}

	city := t.TempDir()
	item, release := leasedStart(t, city, preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, bead),
			tp:   TemplateParams{TemplateName: "worker"},
		},
		cfg: runtime.Config{
			Command: "claude --resume seeded-session",
			WorkDir: info.WorkDir,
		},
	})
	defer release()
	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, store, nil, 10*time.Second, 1)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].err != nil {
		t.Fatalf("start result err = %v, want nil", results[0].err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.State != sessionpkg.StateStartPending {
		t.Fatalf("state = %q, want %q before lifecycle commit", got.State, sessionpkg.StateStartPending)
	}
	updatedBead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if updatedBead.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved before commit", updatedBead.Metadata["pending_create_claim"])
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after prepared start")
	}
}

func TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()

	usedWorker, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{
				Command: "claude --resume seeded",
				WorkDir: t.TempDir(),
			},
		},
		city,
		nil,
		sp,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("startPreparedStartCandidate: %v", err)
	}
	if !usedWorker {
		t.Fatal("usedWorker = false, want true")
	}
	if !sp.IsRunning("legacy-runtime-only") {
		t.Fatal("legacy-runtime-only should be running after prepared start")
	}
	var start runtime.Call
	foundStart := false
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			start = call
			foundStart = true
			break
		}
	}
	if !foundStart {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if start.Name != "legacy-runtime-only" {
		t.Fatalf("start name = %q, want legacy-runtime-only", start.Name)
	}
	if start.Config.Command != "claude --resume seeded" {
		t.Fatalf("start command = %q, want claude --resume seeded", start.Config.Command)
	}
}

func TestPrepareStartCandidateDoesNotRotateIdentityForLiveRuntime(t *testing.T) {
	env := newReconcilerTestEnv(t)
	bead := env.createSessionBead("worker", "worker")
	env.markSessionActive(&bead)
	info := env.sessionInfo(bead.ID)

	const token = "test-token"
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}); err != nil {
		t.Fatalf("starting existing runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     info.ID,
		"GC_INSTANCE_TOKEN": token,
		"GC_RUNTIME_EPOCH":  info.Generation,
	} {
		if err := env.sp.SetMeta("worker", key, value); err != nil {
			t.Fatalf("setting runtime identity %s: %v", key, err)
		}
	}

	lease, release, err := tryRuntimeLease(env.store, env.city, "worker", info.ID, time.Minute)
	if err != nil {
		t.Fatalf("taking runtime lease: %v", err)
	}
	defer release()

	candidate := startCandidate{
		info:  info,
		lease: lease,
		tp: TemplateParams{
			Command:      "codex",
			SessionName:  "worker",
			TemplateName: "worker",
			Hints:        agent.StartupHints{ProcessNames: []string{"codex"}},
		},
	}
	_, err = prepareStartCandidateForCity(candidate, env.city, "test-city", env.cfg, env.sp, env.store, env.clk, io.Discard, nil, dispatchOptionSources{})
	if !errors.Is(err, errStartCandidateRuntimeAlreadyAlive) {
		t.Fatalf("prepareStartCandidateForCity error = %v, want already-alive skip", err)
	}

	got := env.sessionInfo(info.ID)
	if got.InstanceToken != token || got.Generation != info.Generation {
		t.Fatalf("session identity after skipped wake = (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, token, info.Generation)
	}
	if got.MetadataState != "active" || got.PendingCreateStartedAt != "" {
		t.Fatalf("session wake metadata after skipped wake = (state %q, pending_create_started_at %q), want active with no pending start", got.MetadataState, got.PendingCreateStartedAt)
	}
	runtimeToken, err := env.sp.GetMeta("worker", "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatalf("reading runtime identity: %v", err)
	}
	if runtimeToken != got.InstanceToken {
		t.Fatalf("runtime token = %q, session token = %q; warm reuse must keep their identities aligned", runtimeToken, got.InstanceToken)
	}
}

func identityPlannerCandidate(t *testing.T, env *reconcilerTestEnv, state string) (startCandidate, beads.Bead, TemplateParams) {
	t.Helper()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, state)
	tp := TemplateParams{
		Command:      "codex",
		SessionName:  "session-a",
		TemplateName: "template-a",
		Hints:        agent.StartupHints{ProcessNames: []string{"codex"}},
	}
	env.setSessionMetadata(&bead, map[string]string{
		"generation":                      "1",
		"instance_token":                  "row-token",
		"pool_managed":                    "true",
		beadmeta.TriggerBeadIDMetadataKey: "work-1",
	})
	if state == "active" {
		env.setSessionMetadata(&bead, map[string]string{"last_woke_at": env.clk.Now().UTC().Format(time.RFC3339)})
	}
	return startCandidate{info: env.sessionInfo(bead.ID), tp: tp}, bead, tp
}

func startIdentityRuntime(t *testing.T, env *reconcilerTestEnv, candidate startCandidate) {
	t.Helper()
	if err := env.sp.Start(context.Background(), candidate.name(), runtime.Config{
		Command:      "codex",
		ProcessNames: []string{"codex"},
	}); err != nil {
		t.Fatalf("starting existing runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     candidate.info.ID,
		"GC_INSTANCE_TOKEN": candidate.info.InstanceToken,
		"GC_RUNTIME_EPOCH":  candidate.info.Generation,
	} {
		if err := env.sp.SetMeta(candidate.name(), key, value); err != nil {
			t.Fatalf("setting runtime identity %s: %v", key, err)
		}
	}
}

type closeRowOnLivenessProvider struct {
	*runtime.Fake
	store beads.Store
	id    string
}

func (p *closeRowOnLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if err := p.store.Close(p.id); err != nil {
		return runtime.Liveness{}, err
	}
	return runtime.ObserveLiveness(p.Fake, name, processNames), nil
}

func runIdentityPlanner(t *testing.T, env *reconcilerTestEnv, candidate startCandidate, tp TemplateParams, async bool) int {
	return runIdentityPlannerWithProvider(t, env, candidate, tp, async, env.sp)
}

func runIdentityPlannerWithProvider(t *testing.T, env *reconcilerTestEnv, candidate startCandidate, tp TemplateParams, async bool, sp runtime.Provider) int {
	t.Helper()
	options := []startExecutionOption{
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	}
	var tracker *asyncStartTracker
	if async {
		tracker = &asyncStartTracker{}
		options = append(options, withAsyncStartExecution(), withAsyncStartTracker(tracker))
	}
	woken := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg,
		map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second,
		io.Discard, &env.stderr, nil, options...,
	)
	if tracker != nil && !tracker.wait(10*time.Second) {
		t.Fatal("async start tracker did not drain")
	}
	return woken
}

func TestExecutePlannedStartsSkipsLiveSnapshotMismatchWithoutRotatingIdentity(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			// The candidate is the planner's not-alive snapshot. The runtime
			// becomes live before the executor takes its name lease.
			startIdentityRuntime(t, env, candidate)
			if err := setReconcilerDrainAckMetadata(env.sp, candidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
				t.Fatalf("setting drain ack: %v", err)
			}
			cb := defaultSessionCircuitBreaker()
			identity := namedSessionIdentityInfo(candidate.info)
			cb.RecordRestart(identity, env.clk.Now().Add(-time.Minute))
			beforeBreaker := cb.entrySnapshot(identity)

			if got := runIdentityPlanner(t, env, candidate, tp, async); got != 0 {
				t.Fatalf("wake count = %d, want 0 for warm reuse", got)
			}
			got := env.sessionInfo(bead.ID)
			if got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
				t.Fatalf("session identity = (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, candidate.info.InstanceToken, candidate.info.Generation)
			}
			if got.State != sessionpkg.StateActive {
				t.Fatalf("state = %q, want active", got.State)
			}
			if env.sp.CountCalls("Start", candidate.name()) != 1 {
				t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
			}
			if n, _ := countNudges(env.sp); n != 1 {
				t.Fatalf("claim nudge count = %d, want exactly 1", n)
			}
			runtimeToken, err := env.sp.GetMeta(candidate.name(), "GC_INSTANCE_TOKEN")
			if err != nil || runtimeToken != got.InstanceToken {
				t.Fatalf("runtime token = %q (err %v), session token = %q", runtimeToken, err, got.InstanceToken)
			}
			if after := cb.entrySnapshot(identity); !reflect.DeepEqual(after, beforeBreaker) {
				t.Fatalf("breaker snapshot after a skipped start = %#v, want %#v", after, beforeBreaker)
			}
			for _, key := range []string{"GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey} {
				value, err := env.sp.GetMeta(candidate.name(), key)
				if err != nil || value != "" {
					t.Errorf("drain metadata %s = %q (err %v), want cleared", key, value, err)
				}
			}
			raw, err := env.store.Get(bead.ID)
			if err != nil {
				t.Fatalf("reading lease record: %v", err)
			}
			if raw.Metadata[sessionpkg.RuntimeLeaseHolderKey] != "" {
				t.Fatalf("runtime lease holder = %q, want cleared", raw.Metadata[sessionpkg.RuntimeLeaseHolderKey])
			}
		})
	}
}

func TestExecutePlannedStartsDoesNotNudgeHeldOrClosedLiveRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *reconcilerTestEnv, beads.Bead)
	}{
		{name: "held", mutate: func(t *testing.T, env *reconcilerTestEnv, bead beads.Bead) {
			env.setSessionMetadata(&bead, map[string]string{"sleep_intent": "user-hold"})
		}},
		{name: "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			startIdentityRuntime(t, env, candidate)
			sp := runtime.Provider(env.sp)
			if tc.mutate != nil {
				tc.mutate(t, env, bead)
			} else {
				// Close after executePlannedStartsTraced takes the lease record,
				// during its fresh liveness observation, so preparation exercises
				// the locked closed-row check.
				sp = &closeRowOnLivenessProvider{Fake: env.sp, store: env.store, id: bead.ID}
			}
			if got := runIdentityPlannerWithProvider(t, env, candidate, tp, false, sp); got != 0 {
				t.Fatalf("wake count = %d, want 0", got)
			}
			if n, _ := countNudges(env.sp); n != 0 {
				t.Fatalf("claim nudge count = %d, want 0", n)
			}
			if env.sp.CountCalls("Start", candidate.name()) != 1 {
				t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
			}
		})
	}
}

func TestExecutePlannedStartsDefersLivenessProbeErrorAndRestoresBreaker(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, candidate)
	fake := env.sp
	cb := defaultSessionCircuitBreaker()
	identity := namedSessionIdentityInfo(candidate.info)
	cb.RecordRestart(identity, env.clk.Now().Add(-time.Minute))
	beforeBreaker := cb.entrySnapshot(identity)
	// The provider can report a live-looking result and still fail its
	// authoritative liveness observation.
	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg,
		map[string]TemplateParams{tp.SessionName: tp}, &startUnavailableLivenessProvider{Fake: fake}, env.store,
		"test-city", env.city, env.clk, events.Discard, 5*time.Second,
		io.Discard, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0", got)
	}
	if fake.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want only the pre-existing runtime start", fake.CountCalls("Start", candidate.name()))
	}
	if n, _ := countNudges(fake); n != 0 {
		t.Fatalf("claim nudge count = %d, want 0 on an uncertain observation", n)
	}
	if after := cb.entrySnapshot(identity); !reflect.DeepEqual(after, beforeBreaker) {
		t.Fatalf("breaker snapshot after deferred observation = %#v, want %#v", after, beforeBreaker)
	}
	if !strings.Contains(env.stderr.String(), string(TraceOutcomeDeferred)) || strings.Contains(env.stderr.String(), "outcome=failed") {
		t.Fatalf("liveness error should be logged as deferred, stderr=%q", env.stderr.String())
	}
	if got := env.sessionInfo(bead.ID); got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
		t.Fatalf("identity after deferred observation = (%q, %q), want unchanged", got.InstanceToken, got.Generation)
	}
}

func TestExecutePlannedStartsPendingCreateWarmReusePreservesIdentity(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "start-pending")
	env.setSessionMetadata(&bead, map[string]string{"pending_create_claim": "true"})
	candidate.info = env.sessionInfo(bead.ID)
	startIdentityRuntime(t, env, candidate)
	// The existing worker carries the row ID and old token but has no epoch,
	// which the pending-create attribution contract still classifies as ours.
	if err := env.sp.RemoveMeta(candidate.name(), "GC_RUNTIME_EPOCH"); err != nil {
		t.Fatalf("removing runtime epoch: %v", err)
	}
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1 after pending create converges", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != candidate.info.InstanceToken || got.Generation != candidate.info.Generation {
		t.Fatalf("pending-create warm reuse rotated identity to (%q, %q), want unchanged (%q, %q)", got.InstanceToken, got.Generation, candidate.info.InstanceToken, candidate.info.Generation)
	}
	if got.PendingCreateClaim || got.State != sessionpkg.StateActive {
		t.Fatalf("pending-create convergence left claim=%v state=%q, want cleared and active", got.PendingCreateClaim, got.State)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want only the pre-existing runtime start", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsPendingCreateWithoutRuntimeStillPreWakes(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "start-pending")
	env.setSessionMetadata(&bead, map[string]string{"pending_create_claim": "true"})
	candidate.info = env.sessionInfo(bead.ID)
	oldToken, oldGeneration := candidate.info.InstanceToken, candidate.info.Generation
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken == oldToken || got.Generation == oldGeneration {
		t.Fatalf("pending create did not mint identity: got (%q, %q), old (%q, %q)", got.InstanceToken, got.Generation, oldToken, oldGeneration)
	}
	if got.PendingCreateClaim || got.State != sessionpkg.StateActive {
		t.Fatalf("pending create ended claim=%v state=%q, want cleared and active", got.PendingCreateClaim, got.State)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want 1", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsStillRecyclesZombieRuntime(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, _, tp := identityPlannerCandidate(t, env, "active")
	if err := env.sp.Start(context.Background(), candidate.name(), runtime.Config{Command: "codex", ProcessNames: []string{"codex"}}); err != nil {
		t.Fatalf("starting zombie runtime: %v", err)
	}
	env.sp.Zombies[candidate.name()] = true
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 1 {
		t.Fatalf("wake count = %d, want 1 after recycling zombie", got)
	}
	if env.sp.CountCalls("Stop", candidate.name()) != 1 || env.sp.CountCalls("Start", candidate.name()) != 2 {
		t.Fatalf("zombie recycle calls: Stop=%d Start=%d, want one stop and one new start after existing runtime", env.sp.CountCalls("Stop", candidate.name()), env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestExecutePlannedStartsRestoresIdentityForExistingK8sInitializingSession(t *testing.T) {
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	oldToken, oldGeneration := candidate.info.InstanceToken, candidate.info.Generation
	env.sp.StartErrors[candidate.name()] = fmt.Errorf("%w: %w: young pod", runtime.ErrSessionInitializing, runtime.ErrExistingSessionInitializing)
	if got := runIdentityPlanner(t, env, candidate, tp, false); got != 0 {
		t.Fatalf("wake count = %d, want 0 for initializing existing pod", got)
	}
	got := env.sessionInfo(bead.ID)
	if got.InstanceToken != oldToken || got.Generation != oldGeneration {
		t.Fatalf("identity after existing-pod initialization = (%q, %q), want restored (%q, %q)", got.InstanceToken, got.Generation, oldToken, oldGeneration)
	}
	if got.State != sessionpkg.StateActive || got.LastWokeAt != "" {
		t.Fatalf("lifecycle after deferred existing pod = state %q last_woke_at %q, want active with no in-flight marker", got.State, got.LastWokeAt)
	}
	if env.sp.CountCalls("Start", candidate.name()) != 1 {
		t.Fatalf("Start calls = %d, want one initializing response", env.sp.CountCalls("Start", candidate.name()))
	}
}

func TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn(t *testing.T) {
	cityPath := t.TempDir()
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	sp := runtime.NewFake()
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	_, err := startPreparedStartCandidate(context.Background(), preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
			tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
		},
		cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
	}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `rig "aoa" is suspended`) {
		t.Fatalf("error = %v, want suspended rig refusal", err)
	}
	if sp.CountCalls("Start", "aoa--worker-1") != 0 {
		t.Fatal("provider Start ran after suspension landed before spawn")
	}
}

func TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	sp := &blockingStartProvider{Provider: fake, started: make(chan struct{}), release: make(chan struct{})}
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	done := make(chan error, 1)
	go func() {
		_, err := startPreparedStartCandidate(context.Background(), preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
				tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
		done <- err
	}()
	<-sp.started
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	close(sp.release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight start returned %v, want completion", err)
	}
	if !fake.IsRunning("aoa--worker-1") {
		t.Fatal("session crossing the suspension boundary was killed")
	}
	if fake.CountCalls("Stop", "aoa--worker-1") != 0 {
		t.Fatal("suspension killed an in-flight session")
	}
}

func TestStartPreparedStartCandidateDefersWhenLivenessUnavailable(t *testing.T) {
	testCity := t.TempDir()
	sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}

	started, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		testCity,
		nil,
		sp,
		nil,
		nil,
		nil,
		nil,
	)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("startPreparedStartCandidate error = %v, want runtime unavailable", err)
	}
	if started {
		t.Fatal("started = true, want false while runtime absence is unconfirmed")
	}
	if got := sp.CountCalls("Start", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Start calls = %d, want 0", got)
	}
	if got := sp.CountCalls("Stop", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

func TestStartPreparedStartCandidateConvergesFromConfirmedAbsence(t *testing.T) {
	city := t.TempDir()
	sp := &startConfirmedAbsentProvider{Fake: runtime.NewFake()}
	started, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "worker", SessionNameMetadata: "worker"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		city,
		nil, sp, nil, nil, nil, nil,
	)
	if err != nil || !started {
		t.Fatalf("startPreparedStartCandidate = (%v, %v), want started without error", started, err)
	}
	if got := sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("Start calls = %d, want 1 after confirmed absence", got)
	}
}

func TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pendingCreate bool
	}{
		{name: "ordinary wake"},
		{name: "pending create", pendingCreate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
			metadata := map[string]string{
				"session_name":  "worker",
				"template":      "worker",
				"state":         "asleep",
				"last_woke_at":  clk.Now().Format(time.RFC3339),
				"wake_attempts": "2",
			}
			if tc.pendingCreate {
				metadata["state"] = "creating"
				metadata["pending_create_claim"] = "true"
			}
			bead, err := store.Create(beads.Bead{
				ID: "gc-worker", Title: "worker", Type: sessionBeadType,
				Labels: []string{sessionBeadLabel}, Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}
			results := executePreparedStartWaveForCity(
				context.Background(),
				[]preparedStart{{
					candidate: startCandidate{
						info: sessiontest.SeedBead(t, bead),
						tp:   TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
					},
					cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
				}},
				t.TempDir(),
				sp,
				store,
				nil,
				10*time.Second, 1,
			)
			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			result := results[0]
			if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.rateLimitScreen {
				t.Fatalf("result = %#v, want deferred without error, rollback, or rate-limit screening", result)
			}
			if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatal("deferred result counted as committed wake")
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if got.Status != "open" || got.Metadata["wake_attempts"] != "2" {
				t.Fatalf("session after deferral = %#v, want open state and unchanged failure budget", got)
			}
			if tc.pendingCreate && got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("pending_create_claim = %q, want true", got.Metadata["pending_create_claim"])
			}
			if got.Metadata["last_woke_at"] != "" {
				t.Fatalf("last_woke_at = %q, want cleared for retry", got.Metadata["last_woke_at"])
			}
			if starts := sp.CountCalls("Start", "worker"); starts != 0 {
				t.Fatalf("Start calls = %d, want 0", starts)
			}
			if stops := sp.CountCalls("Stop", "worker"); stops != 0 {
				t.Fatalf("Stop calls = %d, want 0", stops)
			}
		})
	}
}

// TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease: a
// bead-less legacy start runs its runtime-only handle under the candidate's
// lease (the name's flock the start already holds), instead of taking that
// flock a second time and failing on itself. Without the candidate's lease,
// a flock another holder has defers the controller's start at once.
func TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease(t *testing.T) {
	cityPath := t.TempDir()
	const name = "worker-1"
	item := func(lease *sessionpkg.RuntimeLease) preparedStart {
		return preparedStart{
			candidate: startCandidate{
				info:  sessionpkg.Info{SessionName: name, SessionNameMetadata: name},
				tp:    TemplateParams{TemplateName: "worker"},
				lease: lease,
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}
	}
	lease, release, err := tryRuntimeLease(nil, cityPath, name, "", 0)
	if err != nil {
		t.Fatalf("taking the candidate's flock: %v", err)
	}
	sp := runtime.NewFake()
	began := time.Now()
	if _, err := startPreparedStartCandidate(context.Background(), item(lease), cityPath, nil, sp, &config.City{}, nil, nil, nil); err != nil {
		t.Fatalf("start under the candidate's lease = %v, want started", err)
	}
	if sp.CountCalls("Start", name) != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("provider Starts = %d after %v, want one at once", sp.CountCalls("Start", name), time.Since(began))
	}
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	_, err = startPreparedStartCandidate(context.Background(), item(nil), cityPath, nil, sp, &config.City{}, nil, nil, nil)
	if !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || errors.Is(err, sessionpkg.ErrSessionStarting) || time.Since(began) > 2*time.Second {
		t.Fatalf("start without the lease under another holder's flock = %v after %v, want ErrRuntimeLeaseBusy at once", err, time.Since(began))
	}
	if sp.CountCalls("Start", name) != 1 {
		t.Fatal("the refused start reached the provider")
	}
	release()
}

// TestStartLeaseNameIsTheStartsName: a start's lease names the runtime the
// start starts: the candidate's own name, or, with none, the row's resolved
// one.
func TestStartLeaseNameIsTheStartsName(t *testing.T) {
	for _, c := range []struct {
		meta, resolved, want string
	}{
		{"worker-1", "worker-1", "worker-1"},
		{"worker-1", "s-gc-1", "worker-1"},
		{"", "s-gc-1", "s-gc-1"},
		{"", "", ""},
	} {
		cand := startCandidate{info: sessionpkg.Info{SessionNameMetadata: c.meta, SessionName: c.resolved}}
		if got := startLeaseName(cand); got != c.want {
			t.Errorf("startLeaseName(meta %q, resolved %q) = %q, want %q", c.meta, c.resolved, got, c.want)
		}
	}
}
