package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"ch/kirari04/videocms/config"
)

func TestPublishLocalTreeReplacesOnlyStagedAssets(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "staging", "file-uuid")
	destination := filepath.Join(root, "published", "file-uuid")

	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(source, "720p", "out.m3u8"), "new-playlist")
	write(filepath.Join(source, "720p", "out0.ts"), "new-segment")
	write(filepath.Join(destination, "720p", "old-segment.ts"), "stale")
	write(filepath.Join(destination, "audio-uuid", "audio.m3u8"), "existing-audio")

	if err := publishLocalTree(context.Background(), source, destination); err != nil {
		t.Fatalf("publishLocalTree: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "720p", "out.m3u8")); err != nil || string(got) != "new-playlist" {
		t.Fatalf("published playlist = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "720p", "old-segment.ts")); !os.IsNotExist(err) {
		t.Fatalf("stale segment should be removed when replacing a quality directory; err = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "audio-uuid", "audio.m3u8")); err != nil || string(got) != "existing-audio" {
		t.Fatalf("unrelated audio should be preserved, got %q, err = %v", got, err)
	}
}

func TestDeleteMediaAtRemovesStagingAndPublishedCopies(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	published := filepath.Join(root, "published")
	for _, path := range []string{filepath.Join(staging, "file-uuid", "720p", "out.m3u8"), filepath.Join(published, "file-uuid", "720p", "out.m3u8")} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("playlist"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{FolderVideoQualitysPriv: staging, FolderVideoQualitysFinal: published, PublisherMode: "local"}
	if err := DeleteMediaAt(context.Background(), cfg, "file-uuid"); err != nil {
		t.Fatalf("DeleteMediaAt: %v", err)
	}
	for _, path := range []string{filepath.Join(staging, "file-uuid"), filepath.Join(published, "file-uuid")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed, err=%v", path, err)
		}
	}
}
