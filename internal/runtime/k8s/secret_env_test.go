package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseSecretEnvProjection(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []secretEnvRef
	}{
		{name: "unset", raw: ""},
		{name: "empty", raw: "   "},
		{name: "empty array", raw: "[]"},
		{
			name: "required reference",
			raw:  `[{"name":"GITEA_TOKEN","secret":"arc-config-gascity","key":"giteaToken"}]`,
			want: []secretEnvRef{{Name: "GITEA_TOKEN", Secret: "arc-config-gascity", Key: "giteaToken"}},
		},
		{
			name: "optional reference",
			raw:  `[{"name":"OPTIONAL_TOKEN","secret":"arc-config-gascity","key":"optional","optional":true}]`,
			want: []secretEnvRef{{Name: "OPTIONAL_TOKEN", Secret: "arc-config-gascity", Key: "optional", Optional: true}},
		},
		{
			name: "preserves order",
			raw:  `[{"name":"FIRST","secret":"first-secret","key":"one"},{"name":"SECOND","secret":"second-secret","key":"two"}]`,
			want: []secretEnvRef{
				{Name: "FIRST", Secret: "first-secret", Key: "one"},
				{Name: "SECOND", Secret: "second-secret", Key: "two"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSecretEnvProjection(tt.raw)
			if err != nil {
				t.Fatalf("parseSecretEnvProjection() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(refs) = %d, want %d: %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("refs[%d] = %#v, want %#v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseSecretEnvProjectionRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "malformed JSON", raw: "[", want: "GC_K8S_SECRET_ENV"},
		{name: "object top level", raw: `{ "name":"GITEA_TOKEN" }`, want: "JSON array"},
		{name: "unknown field", raw: `[{"name":"GITEA_TOKEN","secret":"arc-config","key":"token","extra":true}]`, want: "unknown field"},
		{name: "empty name", raw: `[{"name":"","secret":"arc-config","key":"token"}]`, want: "name must be non-empty"},
		{name: "empty secret", raw: `[{"name":"GITEA_TOKEN","secret":"","key":"token"}]`, want: "secret must be non-empty"},
		{name: "empty key", raw: `[{"name":"GITEA_TOKEN","secret":"arc-config","key":""}]`, want: "key must be non-empty"},
		{name: "invalid env name", raw: `[{"name":"GITEA/TOKEN","secret":"arc-config","key":"token"}]`, want: "invalid environment variable name"},
		{name: "invalid Secret name", raw: `[{"name":"GITEA_TOKEN","secret":"Arc_Config","key":"token"}]`, want: "invalid Secret name"},
		{name: "invalid data key", raw: `[{"name":"GITEA_TOKEN","secret":"arc-config","key":"bad key"}]`, want: "invalid Secret data key"},
		{name: "duplicate name", raw: `[{"name":"GITEA_TOKEN","secret":"first","key":"token"},{"name":"GITEA_TOKEN","secret":"second","key":"token"}]`, want: "duplicate environment variable"},
		{name: "controller-only name", raw: `[{"name":"GC_CONTROLLER_TOKEN","secret":"arc-config","key":"token"}]`, want: "controller-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSecretEnvProjection(tt.raw)
			if err == nil {
				t.Fatal("parseSecretEnvProjection() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestBuildPodEnvSecretProjection(t *testing.T) {
	refs := []secretEnvRef{
		{Name: "GITEA_TOKEN", Secret: "arc-config-gascity", Key: "giteaToken"},
		{Name: "OPTIONAL_TOKEN", Secret: "optional-secret", Key: "token", Optional: true},
	}
	env, err := buildPodEnvWithSecretRefs(map[string]string{
		"GC_AGENT":    "worker",
		"GITEA_TOKEN": "literal-must-be-replaced",
	}, refs, "/workspace", podManagedDoltHost, podManagedDoltPort)
	if err != nil {
		t.Fatalf("buildPodEnvWithSecretRefs() error = %v", err)
	}

	byName := envByName(env)
	for _, want := range refs {
		entries := byName[want.Name]
		if len(entries) != 1 {
			t.Fatalf("%s entries = %d, want exactly one: %#v", want.Name, len(entries), entries)
		}
		entry := entries[0]
		if entry.Value != "" {
			t.Errorf("%s literal Value = %q, want empty", want.Name, entry.Value)
		}
		if entry.ValueFrom == nil || entry.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s missing SecretKeyRef: %#v", want.Name, entry)
		}
		ref := entry.ValueFrom.SecretKeyRef
		if ref.Name != want.Secret || ref.Key != want.Key || ref.Optional == nil || *ref.Optional != want.Optional {
			t.Errorf("%s SecretKeyRef = %#v, want %s/%s optional=%t", want.Name, ref, want.Secret, want.Key, want.Optional)
		}
	}
}

func TestBuildPodEnvSecretProjectionSuppressesLegacyGitHubFallback(t *testing.T) {
	env, err := buildPodEnvWithSecretRefs(map[string]string{"GC_AGENT": "worker", "GITHUB_TOKEN": "literal"}, []secretEnvRef{{
		Name: "GITHUB_TOKEN", Secret: "arc-config-gascity", Key: "giteaToken",
	}}, "/workspace", podManagedDoltHost, podManagedDoltPort)
	if err != nil {
		t.Fatalf("buildPodEnvWithSecretRefs() error = %v", err)
	}
	entries := envByName(env)["GITHUB_TOKEN"]
	if len(entries) != 1 {
		t.Fatalf("GITHUB_TOKEN entries = %d, want one: %#v", len(entries), entries)
	}
	if entries[0].ValueFrom == nil || entries[0].ValueFrom.SecretKeyRef == nil {
		t.Fatalf("GITHUB_TOKEN = %#v, want configured SecretKeyRef", entries[0])
	}
	if entries[0].ValueFrom.SecretKeyRef.Name != "arc-config-gascity" {
		t.Fatalf("GITHUB_TOKEN Secret name = %q, want configured reference", entries[0].ValueFrom.SecretKeyRef.Name)
	}
}

func TestBuildPodEnvPreservesLegacyGitHubFallbackWithoutGenericReference(t *testing.T) {
	env, err := buildPodEnvWithSecretRefs(map[string]string{"GC_AGENT": "worker"}, nil, "/workspace", podManagedDoltHost, podManagedDoltPort)
	if err != nil {
		t.Fatalf("buildPodEnvWithSecretRefs() error = %v", err)
	}
	entries := envByName(env)["GITHUB_TOKEN"]
	if len(entries) != 1 {
		t.Fatalf("GITHUB_TOKEN entries = %d, want one: %#v", len(entries), entries)
	}
	ref := entries[0].ValueFrom.SecretKeyRef
	if ref == nil || ref.Name != "git-credentials" || ref.Key != "token" || ref.Optional == nil || !*ref.Optional {
		t.Fatalf("legacy GITHUB_TOKEN ref = %#v, want optional git-credentials/token", entries[0])
	}
}

func envByName(env []corev1.EnvVar) map[string][]corev1.EnvVar {
	byName := make(map[string][]corev1.EnvVar)
	for _, item := range env {
		byName[item.Name] = append(byName[item.Name], item)
	}
	return byName
}
