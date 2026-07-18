package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestEnsureCityScaffoldCreatesDirectories(t *testing.T) {
	cityDir := t.TempDir()

	if err := ensureCityScaffold(cityDir); err != nil {
		t.Fatalf("ensureCityScaffold(%q): %v", cityDir, err)
	}

	for _, rel := range []string{
		citylayout.RuntimeRoot,
		citylayout.CacheRoot,
		citylayout.SystemRoot,
		filepath.Join(citylayout.RuntimeRoot, "runtime"),
	} {
		if fi, err := os.Stat(filepath.Join(cityDir, rel)); err != nil {
			t.Fatalf("missing directory %q: %v", rel, err)
		} else if !fi.IsDir() {
			t.Fatalf("%q is not a directory", rel)
		}
	}
}
