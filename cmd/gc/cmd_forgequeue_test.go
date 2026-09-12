package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgeQueueHost(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr bool
	}{
		{name: "empty", want: ""},
		{name: "https host", baseURL: "https://forge.example/api/v1", want: "forge.example"},
		{name: "https port", baseURL: "https://forge.example:8443", want: "forge.example"},
		{name: "http rejected", baseURL: "http://forge.example", wantErr: true},
		{name: "missing host", baseURL: "https:///api/v1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := forgeQueueHost(test.baseURL)
			if (err != nil) != test.wantErr {
				t.Fatalf("forgeQueueHost(%q) error = %v, wantErr %v", test.baseURL, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("forgeQueueHost(%q) = %q, want %q", test.baseURL, got, test.want)
			}
		})
	}
}

func TestForgeQueueGitCredentialHelperUsesExplicitHosts(t *testing.T) {
	config := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GITHUB_TOKEN", "github-test-token")
	t.Setenv("GITEA_TOKEN", "gitea-test-token")
	t.Setenv("GC_QUEUE_GITEA_HOST", "forge.example")
	if err := configureForgeQueueGitCredentials(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		host     string
		password string
	}{
		{host: "github.com", password: "github-test-token"},
		{host: "forge.example", password: "gitea-test-token"},
	} {
		t.Run(test.host, func(t *testing.T) {
			cmd := exec.Command("git", "credential", "fill")
			cmd.Stdin = strings.NewReader("protocol=https\nhost=" + test.host + "\n\n")
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("git credential fill: %v", err)
			}
			if !strings.Contains(string(output), "password="+test.password) {
				t.Fatalf("credential helper output = %q", output)
			}
		})
	}
}
