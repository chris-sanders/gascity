package config

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// LoadNamepool reads a namepool file and returns the list of names.
// Blank lines and lines starting with # are skipped.
// Returns nil, nil if path is empty.
func LoadNamepool(fs fsys.FS, path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("loading namepool %q: %w", path, err)
	}
	var names []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		names = append(names, line)
	}
	return names, nil
}
