package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestK8sStoreAttachmentUsesResolvedProviderAndScopeBindings(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "")
	t.Setenv("BEADS_DOLT_SERVER_CA", "")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
	writeK8sRouteTestKubeconfig(t)

	cityRoot := t.TempDir()
	writeK8sAttachmentScope(t, cityRoot, contract.MetadataState{
		Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "citydb",
	}, contract.ConfigState{
		IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	managedPort := startK8sAttachmentManagedStore(t, cityRoot)

	// These are the production registry/provider constructors. The seam-backed
	// providers do not need a live cluster to exercise route selection.
	native, err := buildRuntimeRegistry().New("k8s", config.SessionConfig{}, "city", cityRoot)
	if err != nil {
		t.Fatalf("construct native K8s provider: %v", err)
	}
	hybrid, err := newHybridProvider(config.SessionConfig{RemoteMatch: "remote"}, "city", cityRoot)
	if err != nil {
		t.Fatalf("construct hybrid provider: %v", err)
	}

	if !runtimeLeafRequiresK8sStoreAttachment(native, "city-worker") {
		t.Fatal("native K8s route must require store attachment")
	}
	if runtimeLeafRequiresK8sStoreAttachment(hybrid, "local-worker") {
		t.Fatal("hybrid local route must not require K8s store attachment")
	}
	if !runtimeLeafRequiresK8sStoreAttachment(hybrid, "remote-worker") {
		t.Fatal("hybrid remote route must require K8s store attachment")
	}

	// A native route and a hybrid remote route project the same managed city
	// binding. The hybrid local route must not receive controller attachment
	// assertions, even though its outer provider is hybrid.
	for _, tc := range []struct {
		name     string
		provider runtime.Provider
		session  string
		wantK8s  bool
	}{
		{name: "native K8s", provider: native, session: "city-worker", wantK8s: true},
		{name: "hybrid local", provider: hybrid, session: "local-worker", wantK8s: false},
		{name: "hybrid remote", provider: hybrid, session: "remote-worker", wantK8s: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if runtimeLeafRequiresK8sStoreAttachment(tc.provider, tc.session) {
				activeEnv, err := sessionBackendEnvWithError(cityRoot, "", nil)
				if err != nil {
					t.Fatalf("resolve city backend env: %v", err)
				}
				if err := projectK8sStoreAttachmentEnv(env, activeEnv, cityRoot, cityRoot, nil); err != nil {
					t.Fatalf("project K8s city binding: %v", err)
				}
				if activeEnv["GC_DOLT_PORT"] != managedPort {
					t.Fatalf("resolved city port = %q, want %q", activeEnv["GC_DOLT_PORT"], managedPort)
				}
			}
			if tc.wantK8s {
				if env["GC_K8S_CITY_ATTACH_REQUIRED"] != "1" || env["GC_K8S_CITY_DOLT_MANAGED_LOCAL"] != "1" {
					t.Fatalf("managed city projection = %#v, want required managed-local attachment", env)
				}
				if env["GC_K8S_CITY_DOLT_PORT"] != managedPort {
					t.Fatalf("projected city port = %q, want resolved port %q", env["GC_K8S_CITY_DOLT_PORT"], managedPort)
				}
			} else if len(env) != 0 {
				t.Fatalf("hybrid local route received K8s-only env: %#v", env)
			}
		})
	}
}

func TestK8sStoreAttachmentProjectsCityAndRigIndependently(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("BEADS_DOLT_SERVER_TLS", "")
	t.Setenv("BEADS_DOLT_SERVER_CA", "")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")

	t.Run("managed city and external rig", func(t *testing.T) {
		cityRoot := t.TempDir()
		writeK8sAttachmentScope(t, cityRoot, contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "citydb",
		}, contract.ConfigState{
			IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity,
			EndpointStatus: contract.EndpointStatusVerified,
		})
		managedPort := startK8sAttachmentManagedStore(t, cityRoot)

		rigRoot := filepath.Join(cityRoot, "rigs", "frontend")
		writeK8sAttachmentScope(t, rigRoot, contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "frontenddb",
		}, contract.ConfigState{
			IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified, DoltHost: "rig-db.example.test",
			DoltPort: "4406", DoltUser: "rig-user",
		})
		rigs := []config.Rig{{Name: "frontend", Path: "rigs/frontend", Prefix: "fe"}}
		activeEnv, err := sessionBackendEnvWithError(cityRoot, rigRoot, rigs)
		if err != nil {
			t.Fatalf("resolve active rig backend env: %v", err)
		}
		env := map[string]string{}
		if err := projectK8sStoreAttachmentEnv(env, activeEnv, cityRoot, rigRoot, rigs); err != nil {
			t.Fatalf("project independent city and rig bindings: %v", err)
		}
		if env["GC_K8S_CITY_ATTACH_REQUIRED"] != "1" || env["GC_K8S_CITY_DOLT_MANAGED_LOCAL"] != "1" {
			t.Fatalf("city projection = %#v, want required managed-local city", env)
		}
		if env["GC_K8S_CITY_DOLT_PORT"] != managedPort {
			t.Fatalf("city port = %q, want %q", env["GC_K8S_CITY_DOLT_PORT"], managedPort)
		}
		if env["GC_K8S_ATTACH_REQUIRED"] != "1" || env["GC_K8S_DOLT_MANAGED_LOCAL"] != "0" {
			t.Fatalf("rig projection = %#v, want required external rig", env)
		}
		if activeEnv["GC_DOLT_HOST"] != "rig-db.example.test" || activeEnv["GC_DOLT_PORT"] != "4406" {
			t.Fatalf("external rig endpoint changed: host=%q port=%q", activeEnv["GC_DOLT_HOST"], activeEnv["GC_DOLT_PORT"])
		}
	})

	t.Run("managed city and inherited managed rig", func(t *testing.T) {
		cityRoot := t.TempDir()
		writeK8sAttachmentScope(t, cityRoot, contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "citydb",
		}, contract.ConfigState{
			IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity,
			EndpointStatus: contract.EndpointStatusVerified,
		})
		managedPort := startK8sAttachmentManagedStore(t, cityRoot)

		rigRoot := filepath.Join(cityRoot, "rigs", "frontend")
		writeK8sAttachmentScope(t, rigRoot, contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "frontenddb",
		}, contract.ConfigState{
			IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity,
			EndpointStatus: contract.EndpointStatusVerified,
		})
		rigs := []config.Rig{{Name: "frontend", Path: "rigs/frontend", Prefix: "fe"}}
		activeEnv, err := sessionBackendEnvWithError(cityRoot, rigRoot, rigs)
		if err != nil {
			t.Fatalf("resolve inherited rig backend env: %v", err)
		}
		env := map[string]string{}
		if err := projectK8sStoreAttachmentEnv(env, activeEnv, cityRoot, rigRoot, rigs); err != nil {
			t.Fatalf("project inherited managed rig: %v", err)
		}
		if env["GC_K8S_CITY_ATTACH_REQUIRED"] != "1" || env["GC_K8S_CITY_DOLT_MANAGED_LOCAL"] != "1" {
			t.Fatalf("city projection = %#v, want required managed-local city", env)
		}
		if env["GC_K8S_ATTACH_REQUIRED"] != "1" || env["GC_K8S_DOLT_MANAGED_LOCAL"] != "1" {
			t.Fatalf("rig projection = %#v, want required inherited managed-local rig", env)
		}
		if activeEnv["GC_DOLT_HOST"] != "127.0.0.1" || activeEnv["GC_DOLT_PORT"] != managedPort {
			t.Fatalf("inherited rig endpoint changed: host=%q port=%q", activeEnv["GC_DOLT_HOST"], activeEnv["GC_DOLT_PORT"])
		}
	})

	t.Run("external city and non-Dolt rig", func(t *testing.T) {
		// `http` is served by the linked beads backend. Its workspace carries
		// .beads metadata, but it does not use the controller's Dolt attachment.
		registerLibraryBackendForTest(t, "http")
		cityRoot := t.TempDir()
		writeK8sAttachmentScope(t, cityRoot, contract.MetadataState{
			Database: "dolt", Backend: "dolt", DoltMode: "server", DoltDatabase: "citydb",
		}, contract.ConfigState{
			IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified, DoltHost: "city-db.example.test",
			DoltPort: "4406", DoltUser: "city-user",
		})

		rigRoot := filepath.Join(cityRoot, "rigs", "frontend")
		writeK8sAttachmentScope(t, rigRoot, contract.MetadataState{
			Database: "dolt", Backend: "http", DoltDatabase: "frontenddb",
		}, contract.ConfigState{
			IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified, DoltHost: "beads.example.test",
			DoltPort: "443",
		})
		rigs := []config.Rig{{Name: "frontend", Path: "rigs/frontend", Prefix: "fe"}}
		activeEnv, err := sessionBackendEnvWithError(cityRoot, rigRoot, rigs)
		if err != nil {
			t.Fatalf("resolve non-Dolt rig env: %v", err)
		}
		env := map[string]string{}
		if err := projectK8sStoreAttachmentEnv(env, activeEnv, cityRoot, rigRoot, rigs); err != nil {
			t.Fatalf("project external city and non-Dolt rig: %v", err)
		}
		if env["GC_K8S_CITY_ATTACH_REQUIRED"] != "1" || env["GC_K8S_CITY_DOLT_MANAGED_LOCAL"] != "0" {
			t.Fatalf("external city projection = %#v, want required verified external endpoint", env)
		}
		if env["GC_K8S_CITY_DOLT_HOST"] != "city-db.example.test" || env["GC_K8S_CITY_DOLT_PORT"] != "4406" {
			t.Fatalf("external city endpoint changed: host=%q port=%q", env["GC_K8S_CITY_DOLT_HOST"], env["GC_K8S_CITY_DOLT_PORT"])
		}
		if env["GC_K8S_ATTACH_REQUIRED"] != "0" || env["GC_K8S_DOLT_MANAGED_LOCAL"] != "0" {
			t.Fatalf("non-Dolt rig triggered attachment: %#v", env)
		}
	})
}

func writeK8sRouteTestKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: route-test
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: route-test
  context:
    cluster: route-test
    user: route-test
current-context: route-test
users:
- name: route-test
  user: {}
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("write isolated K8s route kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", path)
}

func writeK8sAttachmentScope(t *testing.T, root string, metadata contract.MetadataState, cfg contract.ConfigState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(root, ".beads", "metadata.json"), metadata); err != nil {
		t.Fatalf("write canonical metadata for %s: %v", root, err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(root, ".beads", "config.yaml"), cfg); err != nil {
		t.Fatalf("write canonical config for %s: %v", root, err)
	}
}

func startK8sAttachmentManagedStore(t *testing.T, cityRoot string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for managed Dolt fixture: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	statePath := filepath.Join(cityRoot, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`, os.Getpid(), port, filepath.Join(cityRoot, ".beads", "dolt"))
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(port)
}
