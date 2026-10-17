package runtime_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

type startupNudgeRouter struct {
	runtime.Provider
	backends []runtime.Backend
	selected runtime.Provider
	known    bool
}

func (p startupNudgeRouter) Backends() []runtime.Backend { return p.backends }
func (p startupNudgeRouter) RouteFor(name string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: name, Provider: p.selected}, Known: p.known}
}

type startupNudgeLeaf struct {
	runtime.Provider
	names []string
	calls []string
}

type startupNudgeLeasedLeaf struct {
	startupNudgeLeaf
	sessionIDs []string
}

type startupNudgeAckLeaf struct {
	runtime.Provider
	acks []string
}

func (p *startupNudgeAckLeaf) AcknowledgeStartupNudgeEvent(_ context.Context, name, intentID string, revision uint64) error {
	p.acks = append(p.acks, name+":"+intentID+":"+strconv.FormatUint(revision, 10))
	return nil
}

func (p *startupNudgeLeasedLeaf) RecoverStartupNudgesForLeasedSession(_ context.Context, name, sessionID string) []runtime.StartupNudgeStatus {
	p.sessionIDs = append(p.sessionIDs, sessionID)
	p.calls = append(p.calls, name)
	return []runtime.StartupNudgeStatus{{SessionName: name, Incarnation: sessionID, Status: "uncertain"}}
}

func (p *startupNudgeLeaf) StartupNudgeSessionNames(context.Context) []string {
	return p.names
}
func (p *startupNudgeLeaf) RecoverStartupNudgesForSession(_ context.Context, name string) []runtime.StartupNudgeStatus {
	p.calls = append(p.calls, name)
	return []runtime.StartupNudgeStatus{{SessionName: name, Status: "uncertain"}}
}

func TestRecoverStartupNudgesUsesCurrentResolvedRoute(t *testing.T) {
	selected := &startupNudgeLeaf{names: []string{"worker"}}
	unselected := &startupNudgeLeaf{names: []string{"worker"}}
	router := startupNudgeRouter{
		backends: []runtime.Backend{{Label: "local", Provider: unselected}, {Label: "remote", Provider: selected}},
		selected: selected,
		known:    true,
	}

	statuses := runtime.RecoverStartupNudges(context.Background(), router)
	if len(statuses) != 1 || statuses[0].SessionName != "worker" {
		t.Fatalf("statuses = %+v, want selected worker status", statuses)
	}
	if len(selected.calls) != 1 || selected.calls[0] != "worker" {
		t.Fatalf("selected backend calls = %v, want worker", selected.calls)
	}
	if len(unselected.calls) != 0 {
		t.Fatalf("unselected backend calls = %v, want none", unselected.calls)
	}
}

func TestRecoverStartupNudgesSkipsUnknownRoute(t *testing.T) {
	leaf := &startupNudgeLeaf{names: []string{"worker"}}
	router := startupNudgeRouter{
		backends: []runtime.Backend{{Label: "remote", Provider: leaf}},
		selected: leaf,
		known:    false,
	}

	if statuses := runtime.RecoverStartupNudges(context.Background(), router); len(statuses) != 1 || statuses[0].Status != "uncertain" || statuses[0].Reason == "" {
		t.Fatalf("statuses = %+v, want visible uncertainty while route is unknown", statuses)
	}
	if len(leaf.calls) != 0 {
		t.Fatalf("calls = %v, want no recovery before route is established", leaf.calls)
	}
}

func TestRecoverStartupNudgeForSessionForwardsLeasedIncarnation(t *testing.T) {
	leaf := &startupNudgeLeasedLeaf{startupNudgeLeaf: startupNudgeLeaf{names: []string{"worker"}}}
	router := startupNudgeRouter{
		backends: []runtime.Backend{{Label: "remote", Provider: leaf}},
		selected: leaf,
		known:    true,
	}
	statuses := runtime.RecoverStartupNudgeForSession(context.Background(), router, "worker", "session-42")
	if len(statuses) != 1 || statuses[0].Incarnation != "session-42" {
		t.Fatalf("statuses = %+v, want leased session incarnation", statuses)
	}
	if len(leaf.sessionIDs) != 1 || leaf.sessionIDs[0] != "session-42" {
		t.Fatalf("leased session IDs = %v, want session-42", leaf.sessionIDs)
	}
}

func TestAcknowledgeStartupNudgeEventUsesCurrentResolvedRoute(t *testing.T) {
	selected := &startupNudgeAckLeaf{}
	unselected := &startupNudgeAckLeaf{}
	router := startupNudgeRouter{
		backends: []runtime.Backend{{Label: "local", Provider: unselected}, {Label: "remote", Provider: selected}},
		selected: selected,
		known:    true,
	}
	if err := runtime.AcknowledgeStartupNudgeEvent(context.Background(), router, "worker", "intent-1", 7); err != nil {
		t.Fatalf("AcknowledgeStartupNudgeEvent: %v", err)
	}
	if len(selected.acks) != 1 || selected.acks[0] != "worker:intent-1:7" {
		t.Fatalf("selected route acknowledgements = %v", selected.acks)
	}
	if len(unselected.acks) != 0 {
		t.Fatalf("unselected route acknowledgements = %v", unselected.acks)
	}
}
