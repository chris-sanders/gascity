package workspacesvc

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestValidateRuntimeSupportRejectsMissingWorkflowContract(t *testing.T) {
	err := ValidateRuntimeSupport([]config.Service{{
		Name:     "review-intake",
		Workflow: config.ServiceWorkflowConfig{Contract: "missing.contract"},
	}})
	if err == nil {
		t.Fatal("expected unsupported workflow contract error")
	}
	if !strings.Contains(err.Error(), `unsupported workflow contract "missing.contract"`) {
		t.Fatalf("error = %v, want unsupported workflow contract", err)
	}
}

func TestValidateRuntimeSupportAcceptsRegisteredWorkflowContract(t *testing.T) {
	contract := uniqueContract(t)
	registerWorkflowContractForTest(t, contract, func(RuntimeContext, config.Service) (Instance, error) {
		return &testInstance{}, nil
	})

	if err := ValidateRuntimeSupport([]config.Service{{
		Name:     "review-intake",
		Workflow: config.ServiceWorkflowConfig{Contract: contract},
	}}); err != nil {
		t.Fatalf("ValidateRuntimeSupport: %v", err)
	}
}

func TestValidateRuntimeSupportAcceptsBuiltinWorkflowContract(t *testing.T) {
	if err := ValidateRuntimeSupport([]config.Service{{
		Name:     "healthz",
		Workflow: config.ServiceWorkflowConfig{Contract: HealthzWorkflowContract},
	}}); err != nil {
		t.Fatalf("ValidateRuntimeSupport: %v", err)
	}
}

func TestValidateRuntimeSupportAcceptsProxyProcess(t *testing.T) {
	if err := ValidateRuntimeSupport([]config.Service{{
		Name: "bridge",
		Kind: "proxy_process",
		Process: config.ServiceProcessConfig{
			Command: []string{"./scripts/start-bridge.sh"},
		},
	}}); err != nil {
		t.Fatalf("ValidateRuntimeSupport: %v", err)
	}
}
