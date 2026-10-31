package unity

import (
	"os"
	"path/filepath"
	"testing"
)

// put writes a file, making the folders above it: what the tests of this package use to lay out a project.
func put(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
