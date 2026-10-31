package services

import (
	"path/filepath"
	"testing"
)

func TestConverterStoredPathStoresDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "qualitys")
	outDir := filepath.Join(root, "file-uuid", "720p")
	got := converterStoredPath(root, outDir)
	want := filepath.Join("file-uuid", "720p")
	if got != want {
		t.Fatalf("converterStoredPath() = %q, want %q", got, want)
	}
}
