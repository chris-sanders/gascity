package workspacesvc

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// HealthzWorkflowContract is a minimal built-in workflow service that exposes
// a simple readiness endpoint for workspace-edge plumbing and pack examples.
const HealthzWorkflowContract = "gc.healthz.v1"

func init() {
	RegisterWorkflowContract(HealthzWorkflowContract, newHealthzWorkflow)
}

type healthzWorkflow struct {
	cityName string
	svcName  string
	contract string
}

func newHealthzWorkflow(rt RuntimeContext, svc config.Service) (Instance, error) {
	return &healthzWorkflow{
		cityName: rt.CityName(),
		svcName:  svc.Name,
		contract: svc.Workflow.Contract,
	}, nil
}

func (h *healthzWorkflow) Status() Status {
	status := Status{
		ServiceName:      h.svcName,
		WorkflowContract: h.contract,
		LocalState:       "ready",
		UpdatedAt:        time.Now().UTC(),
	}
	status.State = "ready"
	return status
}

func (h *healthzWorkflow) HandleHTTP(w http.ResponseWriter, r *http.Request, subpath string) bool {
	switch subpath {
	case "/", "/healthz":
	default:
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"ok":        true,
		"city_name": h.cityName,
		"service":   h.svcName,
		"contract":  h.contract,
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}
	_ = json.NewEncoder(w).Encode(resp)
	return true
}

func (h *healthzWorkflow) Tick(context.Context, time.Time) {}

func (h *healthzWorkflow) Close() error { return nil }
