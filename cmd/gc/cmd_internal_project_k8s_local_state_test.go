package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func TestProjectK8sLocalStateProjectsCanonicalSiteBinding(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "city-src")
	controllerRoot := filepath.Join(root, "city")
	destRoot := filepath.Join(root, "workspace")

	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "city.toml"), []byte("controller city"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destRoot, "city.toml"), []byte("destination city"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeK8sTestSiteBinding(t, sourceRoot, "controller-city", "controller-prefix", []config.Rig{
		{Name: "relative", Path: "rigs/relative"},
		{Name: "absolute", Path: filepath.Join(controllerRoot, "rigs", "absolute")},
	})
	writeK8sTestSiteBinding(t, destRoot, "derived-city", "derived-prefix", nil)

	if err := projectK8sLocalState(sourceRoot, controllerRoot, destRoot); err != nil {
		t.Fatalf("projectK8sLocalState: %v", err)
	}

	binding, err := config.LoadSiteBinding(fsys.OSFS{}, destRoot)
	if err != nil {
		t.Fatal(err)
	}
	if binding.WorkspaceName != "controller-city" || binding.WorkspacePrefix != "controller-prefix" {
		t.Fatalf("workspace identity = %#v, want controller identity", binding)
	}
	if len(binding.Rigs) != 2 {
		t.Fatalf("projected rigs = %#v, want two bindings", binding.Rigs)
	}
	if got, want := binding.Rigs[0], (config.RigSiteBinding{Name: "absolute", Path: filepath.Join(destRoot, "rigs", "absolute")}); got != want {
		t.Fatalf("binding[0] = %#v, want %#v", got, want)
	}
	if got, want := binding.Rigs[1], (config.RigSiteBinding{Name: "relative", Path: filepath.Join(destRoot, "rigs", "relative")}); got != want {
		t.Fatalf("binding[1] = %#v, want %#v", got, want)
	}
	data, err := os.ReadFile(config.SiteBindingPath(destRoot))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), controllerRoot) {
		t.Fatalf("destination site binding retains controller path %q: %s", controllerRoot, data)
	}
	for path, want := range map[string]string{
		filepath.Join(sourceRoot, "city.toml"): "controller city",
		filepath.Join(destRoot, "city.toml"):   "destination city",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s changed to %q", path, got)
		}
	}
}

func TestProjectK8sLocalStatePreservesOmittedDestinationIdentity(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destRoot := filepath.Join(root, "dest")
	controllerRoot := filepath.Join(root, "city")
	writeK8sTestSiteBinding(t, sourceRoot, "", "controller-prefix", nil)
	writeK8sTestSiteBinding(t, destRoot, "destination-city", "destination-prefix", nil)

	if err := projectK8sSiteBinding(fsys.OSFS{}, sourceRoot, controllerRoot, destRoot); err != nil {
		t.Fatalf("projectK8sSiteBinding: %v", err)
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, destRoot)
	if err != nil {
		t.Fatal(err)
	}
	if binding.WorkspaceName != "destination-city" || binding.WorkspacePrefix != "controller-prefix" {
		t.Fatalf("identity = %#v, want destination name and controller prefix", binding)
	}
}

func TestProjectK8sLocalStateValidatesAllBindingsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destRoot := filepath.Join(root, "dest")
	controllerRoot := filepath.Join(root, "city")
	writeK8sTestSiteBinding(t, sourceRoot, "controller-city", "controller-prefix", []config.Rig{
		{Name: "valid", Path: "rigs/valid"},
		{Name: "escape", Path: "../outside"},
	})
	writeK8sTestSiteBinding(t, destRoot, "destination-city", "destination-prefix", []config.Rig{
		{Name: "existing", Path: filepath.Join(destRoot, "rigs", "existing")},
	})
	before, err := os.ReadFile(config.SiteBindingPath(destRoot))
	if err != nil {
		t.Fatal(err)
	}

	err = projectK8sSiteBinding(fsys.OSFS{}, sourceRoot, controllerRoot, destRoot)
	if err == nil || !strings.Contains(err.Error(), "escapes controller City root") {
		t.Fatalf("projectK8sSiteBinding error = %v, want escape failure", err)
	}
	after, err := os.ReadFile(config.SiteBindingPath(destRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("destination binding mutated after validation failure:\nbefore=%safter=%s", before, after)
	}
}

func TestProjectK8sLocalStateRejectsExternalRigAndMalformedSite(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "external absolute", path: filepath.Join(string(filepath.Separator), "other-city", "rig")},
		{name: "relative escape", path: filepath.Join("..", "other-city", "rig")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			sourceRoot := filepath.Join(root, "source")
			destRoot := filepath.Join(root, "dest")
			controllerRoot := filepath.Join(root, "city")
			writeK8sTestSiteBinding(t, sourceRoot, "controller", "prefix", []config.Rig{{Name: "bad", Path: tt.path}})
			writeK8sTestSiteBinding(t, destRoot, "destination", "prefix", nil)
			if err := projectK8sSiteBinding(fsys.OSFS{}, sourceRoot, controllerRoot, destRoot); err == nil {
				t.Fatal("external binding unexpectedly projected")
			}
		})
	}

	t.Run("malformed site", func(t *testing.T) {
		root := t.TempDir()
		sourceRoot := filepath.Join(root, "source")
		destRoot := filepath.Join(root, "dest")
		if err := os.MkdirAll(filepath.Join(sourceRoot, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config.SiteBindingPath(sourceRoot), []byte("not = [valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeK8sTestSiteBinding(t, destRoot, "destination", "prefix", nil)
		if err := projectK8sSiteBinding(fsys.OSFS{}, sourceRoot, filepath.Join(root, "city"), destRoot); err == nil {
			t.Fatal("malformed source site unexpectedly projected")
		}
	})
}

func TestProjectK8sSuspensionStatePreservesExactState(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destRoot := filepath.Join(root, "dest")
	wantUpdatedAt := "2026-09-29T12:00:00Z"
	want := []byte("{\n  \"city\": {\"suspended\": false},\n  \"rigs\": {\"test\": {\"suspended\": true}},\n  \"updated_at\": \"" + wantUpdatedAt + "\"\n}\n")
	writeK8sTestFile(t, citylayout.SuspensionStateFile(sourceRoot), want)
	writeK8sTestFile(t, filepath.Join(sourceRoot, ".gc", "runtime", "other.json"), []byte("unrelated"))

	if err := projectK8sSuspensionState(fsys.OSFS{}, sourceRoot, destRoot); err != nil {
		t.Fatalf("projectK8sSuspensionState: %v", err)
	}
	got, err := os.ReadFile(citylayout.SuspensionStateFile(destRoot))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("projected state = %q, want exact source bytes %q", got, want)
	}
	state, err := suspensionstate.Load(fsys.OSFS{}, destRoot)
	if err != nil {
		t.Fatal(err)
	}
	if state.UpdatedAt.UTC().Format(time.RFC3339) != wantUpdatedAt {
		t.Fatalf("updated_at = %s, want %s", state.UpdatedAt.UTC().Format(time.RFC3339), wantUpdatedAt)
	}
	if got, ok := suspensionstate.ExplicitCity(state); !ok || got {
		t.Fatalf("city override = %v, %v; want explicit resume", got, ok)
	}
	if got, ok := suspensionstate.ExplicitRig(state, "test"); !ok || !got {
		t.Fatalf("rig override = %v, %v; want explicit suspend", got, ok)
	}
	if _, err := os.Stat(filepath.Join(destRoot, ".gc", "runtime", "other.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrelated runtime state was copied, stat error = %v", err)
	}
}

func TestProjectK8sSuspensionStateMissingIsNoOpAndMalformedFails(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destRoot := filepath.Join(root, "dest")
	existing := []byte("existing destination state\n")
	writeK8sTestFile(t, citylayout.SuspensionStateFile(destRoot), existing)
	if err := projectK8sSuspensionState(fsys.OSFS{}, sourceRoot, destRoot); err != nil {
		t.Fatalf("missing source state: %v", err)
	}
	got, err := os.ReadFile(citylayout.SuspensionStateFile(destRoot))
	if err != nil || !bytes.Equal(got, existing) {
		t.Fatalf("missing source changed destination: got=%q err=%v", got, err)
	}

	writeK8sTestFile(t, citylayout.SuspensionStateFile(sourceRoot), []byte("{ malformed"))
	if err := projectK8sSuspensionState(fsys.OSFS{}, sourceRoot, filepath.Join(root, "malformed-dest")); err == nil {
		t.Fatal("malformed source state unexpectedly projected")
	}
}

func TestInternalProjectK8sLocalStateCommandIsRegistered(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "source")
	destRoot := filepath.Join(root, "dest")
	controllerRoot := filepath.Join(root, "city")
	writeK8sTestSiteBinding(t, sourceRoot, "controller", "prefix", []config.Rig{{Name: "test", Path: "rigs/test"}})

	var stdout, stderr bytes.Buffer
	cmd := newInternalCmd(&stdout, &stderr)
	cmd.SetArgs([]string{
		"project-k8s-local-state",
		"--source-root", sourceRoot,
		"--controller-city-root", controllerRoot,
		"--dest-root", destRoot,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("internal command: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "projected Kubernetes local state") {
		t.Fatalf("stdout = %q, want projection confirmation", stdout.String())
	}
}

func writeK8sTestSiteBinding(t *testing.T, root, name, prefix string, rigs []config.Rig) {
	t.Helper()
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, root, name, prefix); err != nil {
		t.Fatal(err)
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, root, rigs); err != nil {
		t.Fatal(err)
	}
}

func writeK8sTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
