package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

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
