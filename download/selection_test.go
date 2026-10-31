package download

import (
	"strings"
	"testing"

	"ch/kirari04/videocms/models"
)

func TestResolveSelectionAllowsHLSMP4WithoutSeparateAudio(t *testing.T) {
	link := &models.Link{File: models.File{Qualitys: []models.Quality{{
		Name: "720p", Type: "hls", Ready: true,
	}}}}
	selection, err := ResolveSelection(link, "720p", ContainerMP4, false, true, nil, nil)
	if err != nil {
		t.Fatalf("ResolveSelection() error = %v", err)
	}
	if len(selection.Audios) != 0 {
		t.Fatalf("selected audios = %d, want none", len(selection.Audios))
	}
}

func TestResolveSelectionHLSMP4AllowsAtMostOneSeparateAudio(t *testing.T) {
	link := &models.Link{File: models.File{
		Qualitys: []models.Quality{{Name: "720p", Type: "hls", Ready: true}},
		Audios: []models.Audio{
			{UUID: "audio-one", Ready: true},
			{UUID: "audio-two", Ready: true},
		},
	}}
	selection, err := ResolveSelection(link, "720p", ContainerMP4, false, true, []string{"audio-one"}, nil)
	if err != nil {
		t.Fatalf("ResolveSelection() with one audio error = %v", err)
	}
	if len(selection.Audios) != 1 || selection.Audios[0].UUID != "audio-one" {
		t.Fatalf("selected audios = %#v, want audio-one", selection.Audios)
	}
	if _, err := ResolveSelection(link, "720p", ContainerMP4, false, true, []string{"audio-one", "audio-two"}, nil); err == nil {
		t.Fatal("ResolveSelection() with two audio tracks succeeded, want error")
	}
}

func TestResolveSelectionNonHLSMP4StillRequiresOneAudio(t *testing.T) {
	link := &models.Link{File: models.File{Qualitys: []models.Quality{{
		Name: "720p", Type: "mp4", Ready: true,
	}}}}
	if _, err := ResolveSelection(link, "720p", ContainerMP4, false, true, nil, nil); err == nil {
		t.Fatal("ResolveSelection() without audio for direct MP4 succeeded, want error")
	}
}

func TestQualityInputPathSupportsLegacyAndDirectoryHLSPaths(t *testing.T) {
	root := "/media/qualitys"
	tests := []struct {
		name string
		path string
		want string
	}{
		{"legacy path includes playlist", "file-uuid/720p/master.m3u8", "/media/qualitys/file-uuid/720p/master.m3u8"},
		{"path stores quality directory", "file-uuid/720p", "/media/qualitys/file-uuid/720p/master.m3u8"},
		{"path is already rooted", "/media/qualitys/file-uuid/720p", "/media/qualitys/file-uuid/720p/master.m3u8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quality := models.Quality{Type: "hls", Path: test.path, OutputFile: "master.m3u8"}
			if got := qualityInputPath(root, quality); got != test.want {
				t.Fatalf("qualityInputPath() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFFmpegArgsMapsEmbeddedHLSAudioAndConvertsWebVTT(t *testing.T) {
	selection := &Selection{
		Container: ContainerMKV,
		QualityRoot: "/media/qualitys",
		Quality: models.Quality{
			Type: "hls", Path: "file-uuid/720p/master.m3u8", OutputFile: "master.m3u8",
		},
		Subtitles: []models.Subtitle{{
			Name: "English", Lang: "eng", Type: "vtt",
			Path: "/media/qualitys/file-uuid/subtitles",
			OutputFile: "subtitle.srt",
		}},
	}
	args := FFmpegArgs(selection, "/tmp/video.mkv", false)
	joined := strings.Join(args, " ")
	for _, expected := range []string{
		"-i /media/qualitys/file-uuid/720p/master.m3u8",
		"-map 0:v:0",
		"-map 0:a:0?",
		"-f webvtt -i /media/qualitys/file-uuid/subtitles/subtitle.srt",
		"-c:s:0 srt",
		"-f matroska",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("FFmpegArgs() missing %q in: %s", expected, joined)
		}
	}
}

func TestFFmpegArgsDoesNotMapEmbeddedHLSAudioWhenSeparateAudioSelected(t *testing.T) {
	selection := &Selection{
		Container: ContainerMKV,
		QualityRoot: "/media/qualitys",
		Quality: models.Quality{
			Type: "hls", Path: "file-uuid/720p", OutputFile: "master.m3u8",
		},
		Audios: []models.Audio{{
			Name: "English", Lang: "eng", Path: "/media/audio", OutputFile: "audio.m3u8",
		}},
	}
	joined := strings.Join(FFmpegArgs(selection, "/tmp/video.mkv", false), " ")
	if strings.Contains(joined, "0:a:0?") {
		t.Fatalf("embedded HLS audio was mapped despite a selected separate audio track: %s", joined)
	}
	if !strings.Contains(joined, "-map 1:a:0") {
		t.Fatalf("selected separate audio was not mapped: %s", joined)
	}
}
