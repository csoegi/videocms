package logic

import (
	"testing"

	"ch/kirari04/videocms/models"
)

func TestDerivePublicationStatus(t *testing.T) {
	tests := []struct {
		name       string
		file       models.File
		taskStatus string
		hasTask    bool
		want       string
	}{
		{
			name:       "publisher task ready",
			file:       models.File{Qualitys: []models.Quality{{Ready: true}}},
			taskStatus: "ready",
			hasTask:    true,
			want:       "ready",
		},
		{
			name:       "publisher task pending",
			file:       models.File{Qualitys: []models.Quality{{Ready: true}}},
			taskStatus: "pending",
			hasTask:    true,
			want:       "processing",
		},
		{
			name:       "publisher task failed",
			file:       models.File{Qualitys: []models.Quality{{Publishing: true}}},
			taskStatus: "failed",
			hasTask:    true,
			want:       "failed",
		},
		{
			name: "legacy video with all assets ready",
			file: models.File{Qualitys: []models.Quality{{Ready: true}}, Audios: []models.Audio{{Ready: true}}},
			want: "ready",
		},
		{
			name:       "republishing a previously published video",
			file:       models.File{Qualitys: []models.Quality{{Ready: true, Publishing: true}}},
			taskStatus: "ready",
			hasTask:    true,
			want:       "processing",
		},
		{
			name:       "active encoding supersedes stale failed task",
			file:       models.File{Qualitys: []models.Quality{{Encoding: true}}},
			taskStatus: "failed",
			hasTask:    true,
			want:       "processing",
		},
		{
			name: "legacy failed asset",
			file: models.File{Qualitys: []models.Quality{{Failed: true}}},
			want: "failed",
		},
		{
			name: "legacy incomplete asset",
			file: models.File{Qualitys: []models.Quality{{Ready: false}}},
			want: "processing",
		},
		{
			name: "no assets and no task",
			file: models.File{},
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := derivePublicationStatus(tt.file, tt.taskStatus, tt.hasTask); got != tt.want {
				t.Fatalf("derivePublicationStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
