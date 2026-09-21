package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var codexFunctionalInputs = []string{
	"contrib/k8s/Dockerfile.agent",
	"contrib/k8s/install-codex-standalone.sh",
	"contrib/k8s/test-codex-runtime.sh",
	".github/workflows/gascity-images.yml",
	".github/workflows/container-scan.yml",
	"scripts/worker_inference_setup.py",
	"renovate.json",
}

func codexVersion(t *testing.T, root string) string {
	t.Helper()
	content := readFile(t, root, "contrib/k8s/codex-runtime/version.env")
	match := regexp.MustCompile(`^CODEX_VERSION=([0-9]+\.[0-9]+\.[0-9]+)$`).FindStringSubmatch(strings.TrimSpace(content))
	if len(match) != 2 {
		t.Fatalf("version.env must contain exactly one stable CODEX_VERSION assignment, got %q", content)
	}
	return match[1]
}

func validateCodexSingleSource(t *testing.T, root string) {
	t.Helper()
	version := codexVersion(t, root)

	for _, path := range []string{
		"contrib/k8s/codex-runtime/package.json",
		"contrib/k8s/codex-runtime/package-lock.json",
	} {
		if _, err := os.Stat(root + "/" + path); !os.IsNotExist(err) {
			t.Errorf("Codex npm artifact %s still exists", path)
		}
	}

	for _, path := range codexFunctionalInputs {
		if strings.Contains(readFile(t, root, path), version) {
			t.Errorf("functional input %s contains the canonical Codex version %s", path, version)
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
	for _, forbidden := range []string{"FROM node:", "npm ci", "package-lock", "CODEX_MANAGED_PACKAGE_ROOT"} {
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
		if strings.Contains(workflow, "package-lock") || strings.Contains(workflow, "@openai/codex") {
			t.Errorf("%s contains a duplicate Codex npm/current-version authority", workflowPath)
		}
	}
}

func TestCodexStandalonePackagingIsSingleSourceAndReusable(t *testing.T) {
	root := repoRoot(t)
	validateCodexSingleSource(t, root)

	// A Renovate update must be equivalent to changing only version.env. Run the
	// same contract against a temporary fixture with a different stable value.
	fixture := t.TempDir()
	for _, path := range append(codexFunctionalInputs, "contrib/k8s/codex-runtime/version.env") {
		content := readFile(t, root, path)
		destination := filepath.Join(fixture, path)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			t.Fatalf("create fixture directory for %s: %v", path, err)
		}
		if path == "contrib/k8s/codex-runtime/version.env" {
			content = "CODEX_VERSION=9.8.7\n"
		}
		if err := os.WriteFile(destination, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", path, err)
		}
	}
	validateCodexSingleSource(t, fixture)
}
