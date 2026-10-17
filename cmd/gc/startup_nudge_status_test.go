package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type startupNudgeEventAckProvider struct {
	runtime.Provider
	acks []uint64
	err  error
}

func (p *startupNudgeEventAckProvider) AcknowledgeStartupNudgeEvent(_ context.Context, _, _ string, revision uint64) error {
	p.acks = append(p.acks, revision)
	return p.err
}

func TestCityStatusExposesUnresolvedStartupNudgeWithoutPromptText(t *testing.T) {
	snapshot := cityStatusSnapshot{
		CityName:   "city",
		CityPath:   "/city",
		Controller: ControllerJSON{Running: true},
		StartupNudgeDeliveries: []runtime.StartupNudgeStatus{{
			SessionName: "worker", IntentID: "immutable-id", Incarnation: "instance-42",
			ProviderFamily: "claude", Status: "uncertain", Reason: "submit_result_ambiguous",
		}},
		Summary: StatusSummaryJSON{TotalAgents: 1, RunningAgents: 1},
	}

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)
	if !status.Health.Degraded || !containsExactString(status.Health.Signals, "startup_nudge_delivery_unresolved") {
		t.Fatalf("health = %#v, want degraded startup-nudge signal", status.Health)
	}
	if len(status.StartupNudgeDeliveries) != 1 || status.StartupNudgeDeliveries[0].IntentID != "immutable-id" {
		t.Fatalf("startup_nudge_deliveries = %#v", status.StartupNudgeDeliveries)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "prompt text") {
		t.Fatalf("status JSON leaked prompt content: %s", data)
	}
	if !strings.Contains(string(data), `"startup_nudge_deliveries"`) {
		t.Fatalf("status JSON omitted delivery diagnostics: %s", data)
	}

	var rendered bytes.Buffer
	renderCityStatusText(snapshot, nil, &rendered)
	for _, want := range []string{"Startup prompt delivery needs attention:", "intent=immutable-id", "status=uncertain"} {
		if !strings.Contains(rendered.String(), want) {
			t.Errorf("text status omitted %q: %s", want, rendered.String())
		}
	}
}

func containsExactString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRecordStartupNudgeRecoveryAcknowledgesOnlyAfterEventAppend(t *testing.T) {
	status := runtime.StartupNudgeStatus{
		SessionName: "worker", IntentID: "intent-1", ProviderFamily: "claude",
		Incarnation: "instance-42", Status: "uncertain", Reason: "submit_result_ambiguous",
		CheckpointRevision: 9, Changed: true,
	}

	t.Run("event append succeeds", func(t *testing.T) {
		provider := &startupNudgeEventAckProvider{}
		recorder := events.NewFake()
		var stderr bytes.Buffer
		cr := &CityRuntime{sp: provider, rec: recorder, stderr: &stderr}

		cr.recordStartupNudgeRecovery([]runtime.StartupNudgeStatus{status})

		if len(recorder.Events) != 1 || recorder.Events[0].Type != startupNudgeUncertainEvent {
			t.Fatalf("events = %+v, want one uncertainty event", recorder.Events)
		}
		if len(provider.acks) != 1 || provider.acks[0] != status.CheckpointRevision {
			t.Fatalf("checkpoint acknowledgements = %v, want revision %d", provider.acks, status.CheckpointRevision)
		}
	})

	t.Run("event append fails", func(t *testing.T) {
		provider := &startupNudgeEventAckProvider{}
		recorder := events.NewFailFake()
		var stderr bytes.Buffer
		cr := &CityRuntime{sp: provider, rec: recorder, stderr: &stderr}

		cr.recordStartupNudgeRecovery([]runtime.StartupNudgeStatus{status})

		if len(provider.acks) != 0 {
			t.Fatalf("checkpoint acknowledgements = %v, want none after failed event append", provider.acks)
		}
		if !strings.Contains(stderr.String(), "could not be persisted; it will be retried") {
			t.Fatalf("stderr = %q, want visible retry diagnostic", stderr.String())
		}
	})
}
