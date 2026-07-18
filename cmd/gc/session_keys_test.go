package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionKeyRoundTrip(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}

	sessionID := "abc123"
	key := "resume-token-xyz"

	// Write key.
	if err := writeSessionKey(cityPath, sessionID, key); err != nil {
		t.Fatalf("writeSessionKey: %v", err)
	}

	// Read key.
	got, err := readSessionKey(cityPath, sessionID)
	if err != nil {
		t.Fatalf("readSessionKey: %v", err)
	}
	if got != key {
		t.Errorf("readSessionKey = %q, want %q", got, key)
	}

	// Verify file permissions.
	keyPath, pathErr := sessionKeyPath(cityPath, sessionID)
	if pathErr != nil {
		t.Fatalf("sessionKeyPath: %v", pathErr)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != secretFilePerm {
		t.Errorf("key file perm = %o, want %o", perm, secretFilePerm)
	}

	// Verify secrets dir permissions.
	dirInfo, err := os.Stat(filepath.Join(cityPath, ".gc", secretsDir))
	if err != nil {
		t.Fatalf("stat secrets dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != secretsDirPerm {
		t.Errorf("secrets dir perm = %o, want %o", perm, secretsDirPerm)
	}
}

func TestReadSessionKey_NotFound(t *testing.T) {
	cityPath := t.TempDir()
	got, err := readSessionKey(cityPath, "nonexistent")
	if err != nil {
		t.Fatalf("readSessionKey: %v", err)
	}
	if got != "" {
		t.Errorf("readSessionKey = %q, want empty", got)
	}
}

func TestRemoveSessionKey(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}

	sessionID := "def456"
	if err := writeSessionKey(cityPath, sessionID, "token"); err != nil {
		t.Fatal(err)
	}

	if err := removeSessionKey(cityPath, sessionID); err != nil {
		t.Fatalf("removeSessionKey: %v", err)
	}

	// Verify file is gone.
	got, err := readSessionKey(cityPath, sessionID)
	if err != nil {
		t.Fatalf("readSessionKey after remove: %v", err)
	}
	if got != "" {
		t.Errorf("readSessionKey after remove = %q, want empty", got)
	}
}

func TestRemoveSessionKey_NotFound(t *testing.T) {
	cityPath := t.TempDir()
	// Should not error on nonexistent file.
	if err := removeSessionKey(cityPath, "nonexistent"); err != nil {
		t.Fatalf("removeSessionKey nonexistent: %v", err)
	}
}

func TestSessionKeyPath_Traversal(t *testing.T) {
	tests := []string{
		"../etc/passwd",
		"foo/bar",
		"..\\windows",
		".",
	}
	for _, id := range tests {
		_, err := sessionKeyPath("/tmp/city", id)
		if err == nil {
			t.Errorf("sessionKeyPath(%q) should fail for path traversal", id)
		}
	}
}

func TestSessionKey_TraversalBlocked(t *testing.T) {
	cityPath := t.TempDir()
	if err := writeSessionKey(cityPath, "../escape", "token"); err == nil {
		t.Error("writeSessionKey should reject path traversal")
	}
	if _, err := readSessionKey(cityPath, "../escape"); err == nil {
		t.Error("readSessionKey should reject path traversal")
	}
	if err := removeSessionKey(cityPath, "../escape"); err == nil {
		t.Error("removeSessionKey should reject path traversal")
	}
}
