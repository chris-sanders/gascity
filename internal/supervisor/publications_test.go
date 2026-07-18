package supervisor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCityPublicationRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publications.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 1,
  "cities": {
    "/workspace/demo": {
      "services": [
        {
          "service_name": "github-webhook",
          "visibility": "public",
          "url": "https://github-webhook--acme--deadbeef.apps.example.com"
        }
      ]
    }
  }
}
`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	refs, exists, err := LoadCityPublicationRefs(path, "/workspace/demo")
	if err != nil {
		t.Fatalf("LoadCityPublicationRefs: %v", err)
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
	ref, ok := refs["github-webhook"]
	if !ok {
		t.Fatal("github-webhook ref missing")
	}
	if ref.Visibility != "public" {
		t.Fatalf("Visibility = %q, want public", ref.Visibility)
	}
	if ref.URL != "https://github-webhook--acme--deadbeef.apps.example.com" {
		t.Fatalf("URL = %q, want hosted route", ref.URL)
	}
}

func TestLoadCityPublicationRefsMissingFile(t *testing.T) {
	refs, exists, err := LoadCityPublicationRefs(filepath.Join(t.TempDir(), "missing.json"), "/workspace/demo")
	if err != nil {
		t.Fatalf("LoadCityPublicationRefs: %v", err)
	}
	if exists {
		t.Fatal("exists = true, want false")
	}
	if refs != nil {
		t.Fatalf("refs = %#v, want nil", refs)
	}
}

func TestLoadCityPublicationRefsRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publications.json")
	if err := os.WriteFile(path, []byte(`{"version":2}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, exists, err := LoadCityPublicationRefs(path, "/workspace/demo")
	if err == nil {
		t.Fatal("LoadCityPublicationRefs error = nil, want unsupported version")
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
}

func TestLoadCityPublicationRefsMissingCityKeepsAuthoritativeStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publications.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 1,
  "cities": {
    "/workspace/other": { "services": [] }
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	refs, exists, err := LoadCityPublicationRefs(path, "/workspace/demo")
	if err != nil {
		t.Fatalf("LoadCityPublicationRefs: %v", err)
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want empty", refs)
	}
}

func TestLoadCityPublicationRefsNormalizesStoredCityKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publications.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 1,
  "cities": {
    "/workspace/demo/": {
      "services": [
        {
          "service_name": "github-webhook",
          "visibility": "public",
          "url": "https://github-webhook--acme--deadbeef.apps.example.com"
        }
      ]
    }
  }
}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	refs, exists, err := LoadCityPublicationRefs(path, "/workspace/demo")
	if err != nil {
		t.Fatalf("LoadCityPublicationRefs: %v", err)
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
	if refs["github-webhook"].URL != "https://github-webhook--acme--deadbeef.apps.example.com" {
		t.Fatalf("URL = %q, want normalized-city lookup", refs["github-webhook"].URL)
	}
}

func TestLoadCityPublicationRefsRejectsMissingVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "publications.json")
	if err := os.WriteFile(path, []byte(`{"cities":{}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, exists, err := LoadCityPublicationRefs(path, "/workspace/demo")
	if err == nil {
		t.Fatal("LoadCityPublicationRefs error = nil, want unsupported version")
	}
	if !exists {
		t.Fatal("exists = false, want true")
	}
}
