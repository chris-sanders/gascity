package scripts_test

import (
	"os"
	"strings"
	"testing"
)

func TestCodexStandalonePackagingIsSingleSourceAndReusable(t *testing.T) {
	root := repoRoot(t)
	version := readFile(t, root, "contrib/k8s/codex-runtime/version.env")
	if strings.TrimSpace(version) != "CODEX_VERSION=0.155.1" {
		t.Fatalf("version.env must contain the reviewed stable version as its only input, got %q", version)
	}

	for _, path := range []string{
		"contrib/k8s/codex-runtime/package.json",
		"contrib/k8s/codex-runtime/package-lock.json",
	} {
		if _, err := os.Stat(root + "/" + path); !os.IsNotExist(err) {
			t.Errorf("Codex npm artifact %s still exists", path)
		}
	}

	helper := readFile(t, root, "contrib/k8s/install-codex-standalone.sh")
	for _, want := range []string{
		"release.json",
		"codex-package_SHA256SUMS",
		"sha256sum",
		"rust-v${release_version}",
		"codex-package-${target}.tar.gz",
		"bin/codex-code-mode-host",
		"codex-path/rg",
		"codex-resources/bwrap",
		"codex-cli ${release_version}",
		"github_api_root",
	} {
		if !strings.Contains(helper, want) {
			t.Errorf("standalone helper missing %q", want)
		}
	}
	if strings.Contains(helper, "curl") && strings.Contains(helper, "| sh") {
		t.Error("standalone helper must not pipe curl into a shell")
	}

	dockerfile := readFile(t, root, "contrib/k8s/Dockerfile.agent")
	for _, want := range []string{
		"install-codex-standalone",
		"codex-runtime/version.env",
		"/opt/gascity-codex/standalone",
		"codex-code-mode-host",
		"codex-resources/bwrap",
		"command -v node",
		"command -v npm",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile.agent missing standalone contract %q", want)
		}
	}
	for _, forbidden := range []string{"FROM node:", "npm ci", "package-lock", "CODEX_MANAGED_PACKAGE_ROOT", "0.155.1"} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("Dockerfile.agent retains forbidden Codex packaging input %q", forbidden)
		}
	}

	worker := readFile(t, root, "scripts/worker_inference_setup.py")
	for _, want := range []string{"version.env", "install-codex-standalone.sh", "install_codex_standalone", "CODEX_CLI_VERSION"} {
		if !strings.Contains(worker, want) {
			t.Errorf("worker setup missing standalone Codex path %q", want)
		}
	}
	if strings.Contains(worker, "@openai/codex") {
		t.Error("worker setup still installs Codex through npm")
	}

	renovate := readFile(t, root, "renovate.json")
	for _, want := range []string{"codex-runtime/version", "github-releases", "openai/codex", "rust-v(?<version>"} {
		if !strings.Contains(renovate, want) {
			t.Errorf("Renovate Codex manager missing %q", want)
		}
	}
	if strings.Contains(renovate, "contrib/k8s/codex-runtime/package.json") || strings.Contains(renovate, "@openai/codex") {
		t.Error("Renovate still owns Codex through npm")
	}

	for _, workflowPath := range []string{".github/workflows/container-scan.yml", ".github/workflows/gascity-images.yml"} {
		workflow := readFile(t, root, workflowPath)
		if !strings.Contains(workflow, "codex-runtime/version.env") || !strings.Contains(workflow, "test-codex-runtime.sh") {
			t.Errorf("%s does not read version.env and reuse the real-binary contract", workflowPath)
		}
		if strings.Contains(workflow, "0.155.1") || strings.Contains(workflow, "package-lock") || strings.Contains(workflow, "@openai/codex") {
			t.Errorf("%s contains a duplicate Codex npm/current-version authority", workflowPath)
		}
	}
}
