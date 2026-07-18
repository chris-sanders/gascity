package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadNamepool_ValidFile(t *testing.T) {
	fs := &fsys.Fake{
		Files: map[string][]byte{
			"/pools/mad-max.txt": []byte("furiosa\nnux\nslit\nrictus\n"),
		},
	}
	names, err := LoadNamepool(fs, "/pools/mad-max.txt")
	if err != nil {
		t.Fatalf("LoadNamepool: %v", err)
	}
	want := []string{"furiosa", "nux", "slit", "rictus"}
	if len(names) != len(want) {
		t.Fatalf("len = %d, want %d", len(names), len(want))
	}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, want[i])
		}
	}
}

func TestLoadNamepool_SkipsBlanksAndComments(t *testing.T) {
	fs := &fsys.Fake{
		Files: map[string][]byte{
			"/pools/test.txt": []byte("# Theme names\nfuriosa\n\n# Another comment\nnux\n\n"),
		},
	}
	names, err := LoadNamepool(fs, "/pools/test.txt")
	if err != nil {
		t.Fatalf("LoadNamepool: %v", err)
	}
	want := []string{"furiosa", "nux"}
	if len(names) != len(want) {
		t.Fatalf("len = %d, want %d", len(names), len(want))
	}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, want[i])
		}
	}
}

func TestLoadNamepool_EmptyPath(t *testing.T) {
	fs := &fsys.Fake{}
	names, err := LoadNamepool(fs, "")
	if err != nil {
		t.Fatalf("LoadNamepool: %v", err)
	}
	if names != nil {
		t.Errorf("got %v, want nil", names)
	}
}

func TestLoadNamepool_MissingFile(t *testing.T) {
	fs := &fsys.Fake{}
	_, err := LoadNamepool(fs, "/no/such/file.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadNamepool_OnlyComments(t *testing.T) {
	fs := &fsys.Fake{
		Files: map[string][]byte{
			"/pools/empty.txt": []byte("# just comments\n# nothing else\n"),
		},
	}
	names, err := LoadNamepool(fs, "/pools/empty.txt")
	if err != nil {
		t.Fatalf("LoadNamepool: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("len = %d, want 0", len(names))
	}
}

func TestLoadNamepool_TrimsWhitespace(t *testing.T) {
	fs := &fsys.Fake{
		Files: map[string][]byte{
			"/pools/spaces.txt": []byte("  furiosa  \n\tnux\t\n"),
		},
	}
	names, err := LoadNamepool(fs, "/pools/spaces.txt")
	if err != nil {
		t.Fatalf("LoadNamepool: %v", err)
	}
	want := []string{"furiosa", "nux"}
	if len(names) != len(want) {
		t.Fatalf("len = %d, want %d", len(names), len(want))
	}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, name, want[i])
		}
	}
}
