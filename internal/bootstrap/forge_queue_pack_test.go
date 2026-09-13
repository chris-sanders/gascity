package bootstrap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestForgeQueuePackScopesWorkerPerAgent(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	bootstrapDir := filepath.Dir(filename)
	pack, err := os.ReadFile(filepath.Join(bootstrapDir, "packs", "forge-queue", "pack.toml"))
	if err != nil {
		t.Fatalf("read forge-queue pack.toml: %v", err)
	}
	if strings.Contains(string(pack), "scope = \"rig\"") {
		t.Fatal("forge-queue pack.toml must not use unsupported agent_defaults.scope; scope belongs on the worker agent")
	}

	worker, err := os.ReadFile(filepath.Join(bootstrapDir, "packs", "forge-queue", "agents", "worker", "agent.toml"))
	if err != nil {
		t.Fatalf("read forge-queue worker agent.toml: %v", err)
	}
	if !strings.Contains(string(worker), "scope = \"rig\"") {
		t.Fatal("forge-queue worker agent.toml must retain rig scope")
	}
}
