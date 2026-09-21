package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const standaloneTestVersion = "1.2.3"
const standaloneTestTarget = "x86_64-unknown-linux-musl"

type standaloneRelease struct {
	packageArchive []byte
	checksums      []byte
	metadata       []byte
}

type standaloneCase struct {
	name            string
	openaiMetadata  string
	openaiPackage   string
	githubMetadata  string
	githubPackage   string
	wantSuccess     bool
	wantSource      string
	wantNoHTTP      bool
	wantExactGitHub bool
}

func standalonePackage(t *testing.T, marker string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gzipWriter := gzip.NewWriter(&archive)
	tarWriter := tar.NewWriter(gzipWriter)
	files := map[string]struct {
		body string
		mode int64
	}{
		"codex-package.json": {
			body: fmt.Sprintf(`{"version":"%s","target":"%s"}`, standaloneTestVersion, standaloneTestTarget),
			mode: 0o644,
		},
		"bin/codex": {
			body: fmt.Sprintf("#!/bin/sh\nprintf 'codex-cli %s\\n'\n", standaloneTestVersion),
			mode: 0o755,
		},
		"bin/codex-code-mode-host": {body: "#!/bin/sh\nexit 0\n", mode: 0o755},
		"codex-path/rg":            {body: "#!/bin/sh\nexit 0\n", mode: 0o755},
		"codex-resources/bwrap":    {body: "#!/bin/sh\nexit 0\n", mode: 0o755},
		"marker.txt":               {body: marker, mode: 0o644},
	}
	for name, file := range files {
		header := &tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.body))}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarWriter, file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func standaloneReleaseFixture(t *testing.T, marker string) standaloneRelease {
	t.Helper()
	packageName := "codex-package-" + standaloneTestTarget + ".tar.gz"
	checksumsName := "codex-package_SHA256SUMS"
	packageArchive := standalonePackage(t, marker)
	checksums := []byte(fmt.Sprintf("%s  %s\n", digest(packageArchive), packageName))
	metadata, err := json.Marshal(map[string]any{
		"tag_name": "rust-v" + standaloneTestVersion,
		"assets": []map[string]string{
			{"name": packageName, "digest": "sha256:" + digest(packageArchive)},
			{"name": checksumsName, "digest": "sha256:" + digest(checksums)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return standaloneRelease{packageArchive: packageArchive, checksums: checksums, metadata: metadata}
}

func TestCodexStandaloneHelperUsesCoherentExactReleaseFallbacks(t *testing.T) {
	openai := standaloneReleaseFixture(t, "openai-package")
	github := standaloneReleaseFixture(t, "github-package")
	packageName := "codex-package-" + standaloneTestTarget + ".tar.gz"
	checksumsName := "codex-package_SHA256SUMS"

	cases := []standaloneCase{
		{name: "openai happy path", openaiMetadata: "good", openaiPackage: "good", githubMetadata: "unused", githubPackage: "unused", wantSuccess: true, wantSource: "openai"},
		{name: "openai metadata unavailable", openaiMetadata: "missing", openaiPackage: "unused", githubMetadata: "good", githubPackage: "good", wantSuccess: true, wantSource: "github", wantExactGitHub: true},
		{name: "openai package unavailable", openaiMetadata: "good", openaiPackage: "missing", githubMetadata: "good", githubPackage: "good", wantSuccess: true, wantSource: "github", wantExactGitHub: true},
		{name: "openai package digest mismatch", openaiMetadata: "good", openaiPackage: "bad", githubMetadata: "good", githubPackage: "good", wantSuccess: true, wantSource: "github", wantExactGitHub: true},
		{name: "both sources fail", openaiMetadata: "good", openaiPackage: "missing", githubMetadata: "good", githubPackage: "missing", wantSuccess: false, wantExactGitHub: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var requestsMu sync.Mutex
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requestsMu.Lock()
				requests = append(requests, request.URL.Path)
				requestsMu.Unlock()

				source := ""
				if strings.HasPrefix(request.URL.Path, "/openai/") {
					source = "openai"
				} else if strings.HasPrefix(request.URL.Path, "/github-api/") {
					source = "github-metadata"
				} else if strings.HasPrefix(request.URL.Path, "/github-download/") {
					source = "github"
				}

				switch {
				case request.URL.Path == "/openai/"+standaloneTestVersion+"/release.json":
					if testCase.openaiMetadata != "good" {
						http.NotFound(response, request)
						return
					}
					response.Write(openai.metadata)
				case request.URL.Path == "/github-api/rust-v"+standaloneTestVersion:
					if testCase.githubMetadata != "good" {
						http.NotFound(response, request)
						return
					}
					response.Write(github.metadata)
				case strings.HasPrefix(request.URL.Path, "/openai/"+standaloneTestVersion+"/"):
					if testCase.openaiPackage == "unused" {
						http.NotFound(response, request)
						return
					}
					switch strings.TrimPrefix(request.URL.Path, "/openai/"+standaloneTestVersion+"/") {
					case packageName:
						if testCase.openaiPackage == "missing" {
							http.NotFound(response, request)
						} else if testCase.openaiPackage == "bad" {
							response.Write([]byte("not-a-codex-archive"))
						} else {
							response.Write(openai.packageArchive)
						}
					case checksumsName:
						response.Write(openai.checksums)
					default:
						http.NotFound(response, request)
					}
				case strings.HasPrefix(request.URL.Path, "/github-download/rust-v"+standaloneTestVersion+"/"):
					if testCase.githubPackage == "unused" {
						http.NotFound(response, request)
						return
					}
					switch strings.TrimPrefix(request.URL.Path, "/github-download/rust-v"+standaloneTestVersion+"/") {
					case packageName:
						if testCase.githubPackage == "missing" {
							http.NotFound(response, request)
						} else {
							response.Write(github.packageArchive)
						}
					case checksumsName:
						response.Write(github.checksums)
					default:
						http.NotFound(response, request)
					}
				default:
					t.Fatalf("unexpected %s request path %s", source, request.URL.Path)
				}
			}))
			defer server.Close()

			destination := t.TempDir()
			command := exec.Command(filepath.Join(repoRoot(t), "contrib/k8s/install-codex-standalone.sh"),
				"--version", standaloneTestVersion,
				"--target", standaloneTestTarget,
				"--destination", destination,
			)
			command.Env = append(os.Environ(),
				"CODEX_OPENAI_RELEASE_ROOT="+server.URL+"/openai",
				"CODEX_GITHUB_API_ROOT="+server.URL+"/github-api",
				"CODEX_GITHUB_RELEASE_ROOT="+server.URL+"/github-download",
			)
			output, err := command.CombinedOutput()
			if testCase.wantSuccess && err != nil {
				t.Fatalf("helper failed: %v\n%s", err, output)
			}
			if !testCase.wantSuccess && err == nil {
				t.Fatalf("helper unexpectedly succeeded:\n%s", output)
			}
			if testCase.wantSuccess && !strings.Contains(string(output), "from "+testCase.wantSource) {
				t.Fatalf("helper output did not identify %s source:\n%s", testCase.wantSource, output)
			}
			if testCase.wantExactGitHub {
				requestsMu.Lock()
				requestText := strings.Join(requests, "\n")
				requestsMu.Unlock()
				for _, path := range []string{
					"/github-api/rust-v" + standaloneTestVersion,
					"/github-download/rust-v" + standaloneTestVersion + "/" + checksumsName,
					"/github-download/rust-v" + standaloneTestVersion + "/" + packageName,
				} {
					if !strings.Contains(requestText, path) {
						t.Errorf("fallback did not request exact GitHub asset %s; requests:\n%s", path, requestText)
					}
				}
			}
		})
	}
}

func TestCodexStandaloneHelperRejectsPrereleaseBeforeDownload(t *testing.T) {
	var requestsMu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestsMu.Lock()
		requestCount++
		requestsMu.Unlock()
		http.Error(response, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	command := exec.Command(filepath.Join(repoRoot(t), "contrib/k8s/install-codex-standalone.sh"),
		"--version", "1.2.3-rc.1",
		"--target", standaloneTestTarget,
		"--destination", t.TempDir(),
	)
	command.Env = append(os.Environ(),
		"CODEX_OPENAI_RELEASE_ROOT="+server.URL+"/openai",
		"CODEX_GITHUB_API_ROOT="+server.URL+"/github-api",
		"CODEX_GITHUB_RELEASE_ROOT="+server.URL+"/github-download",
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("prerelease unexpectedly succeeded:\n%s", output)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if requestCount != 0 {
		t.Fatalf("prerelease attempted %d network requests", requestCount)
	}
}
