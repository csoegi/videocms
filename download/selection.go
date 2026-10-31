package download

import (
	"ch/kirari04/videocms/models"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	ContainerMKV = "mkv"
	ContainerMP4 = "mp4"
)

type Selection struct {
	Container     string
	MediaDuration float64
	Quality       models.Quality
	QualityRoot   string
	Audios        []models.Audio
	Subtitles     []models.Subtitle
}

func ResolveSelection(
	dbLink *models.Link,
	qualityName string,
	container string,
	streaming bool,
	customSelection bool,
	audioUUIDs []string,
	subtitleUUIDs []string,
) (*Selection, error) {
	selection := &Selection{
		Container:     container,
		MediaDuration: dbLink.File.Duration,
	}

	qualityFound := false
	for _, quality := range dbLink.File.Qualitys {
		if quality.Name == qualityName && quality.Ready {
			selection.Quality = quality
			qualityFound = true
			break
		}
	}
	if !qualityFound {
		return nil, errors.New("selected quality is not available")
	}

	readyAudios := make(map[string]models.Audio)
	orderedReadyAudios := make([]models.Audio, 0, len(dbLink.File.Audios))
	for _, audio := range dbLink.File.Audios {
		if !audio.Ready {
			continue
		}
		readyAudios[audio.UUID] = audio
		orderedReadyAudios = append(orderedReadyAudios, audio)
	}

	if streaming {
		if len(orderedReadyAudios) > 0 {
			selection.Audios = []models.Audio{orderedReadyAudios[0]}
		}
		return selection, nil
	}

	if !customSelection {
		return nil, errors.New("download selection is required")
	}

	if container == ContainerMP4 {
		if len(subtitleUUIDs) > 0 {
			return nil, errors.New("MP4 downloads do not support subtitle selection")
		}
		if isHLSQuality(selection.Quality) {
			// The HLS playlist may already contain a muxed audio stream. A
			// separate audio row is optional, but MP4 can contain at most one.
			if len(audioUUIDs) > 1 {
				return nil, errors.New("MP4 downloads support at most one selected audio track for HLS")
			}
			if len(audioUUIDs) == 1 {
				audio, exists := readyAudios[audioUUIDs[0]]
				if !exists {
					return nil, errors.New("selected audio track is not available")
				}
				selection.Audios = []models.Audio{audio}
			}
			return selection, nil
		}
		if len(audioUUIDs) != 1 {
			return nil, errors.New("MP4 downloads require exactly one audio track")
		}
		audio, exists := readyAudios[audioUUIDs[0]]
		if !exists {
			return nil, errors.New("selected audio track is not available")
		}
		selection.Audios = []models.Audio{audio}
		return selection, nil
	}

	if container != ContainerMKV {
		return nil, errors.New("unsupported download container")
	}

	audios, err := selectedReadyAudios(audioUUIDs, readyAudios)
	if err != nil {
		return nil, err
	}
	subtitles, err := selectedReadySubtitles(subtitleUUIDs, dbLink.File.Subtitles)
	if err != nil {
		return nil, err
	}
	selection.Audios = audios
	selection.Subtitles = subtitles
	return selection, nil
}

func selectedReadyAudios(uuids []string, ready map[string]models.Audio) ([]models.Audio, error) {
	selected := make([]models.Audio, 0, len(uuids))
	seen := map[string]bool{}
	for _, selectedUUID := range uuids {
		if seen[selectedUUID] {
			continue
		}
		audio, exists := ready[selectedUUID]
		if !exists {
			return nil, errors.New("selected audio track is not available")
		}
		seen[selectedUUID] = true
		selected = append(selected, audio)
	}
	return selected, nil
}

func selectedReadySubtitles(uuids []string, subtitles []models.Subtitle) ([]models.Subtitle, error) {
	ready := make(map[string]models.Subtitle)
	for _, subtitle := range subtitles {
		if subtitle.Ready {
			ready[subtitle.UUID] = subtitle
		}
	}

	selected := make([]models.Subtitle, 0, len(uuids))
	seen := map[string]bool{}
	for _, selectedUUID := range uuids {
		if seen[selectedUUID] {
			continue
		}
		subtitle, exists := ready[selectedUUID]
		if !exists {
			return nil, errors.New("selected subtitle track is not available")
		}
		seen[selectedUUID] = true
		selected = append(selected, subtitle)
	}
	return selected, nil
}

func FFmpegArgs(selection *Selection, outputPath string, reportProgress bool) []string {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", qualityInputPath(selection.QualityRoot, selection.Quality),
	}

	for _, audio := range selection.Audios {
		args = append(args, "-i", filepath.Join(audio.Path, audio.OutputFile))
	}
	for _, subtitle := range selection.Subtitles {
		if isWebVTTSubtitle(subtitle) {
			// Some uploaded SRT files are stored with WebVTT content and a
			// .srt filename. Force the demuxer from the persisted track type.
			args = append(args, "-f", "webvtt")
		}
		args = append(args, "-i", filepath.Join(subtitle.Path, subtitle.OutputFile))
	}

	args = append(args, "-map", "0:v:0")
	if isHLSQuality(selection.Quality) && len(selection.Audios) == 0 {
		// Converted HLS variants are muxed: audio, when present, is in the
		// same playlist as video. Keep it optional for silent HLS sources.
		args = append(args, "-map", "0:a:0?")
	}
	inputIndex := 1
	for audioIndex, audio := range selection.Audios {
		args = append(args,
			"-map", fmt.Sprintf("%d:a:0", inputIndex),
			fmt.Sprintf("-metadata:s:a:%d", audioIndex), "language="+audio.Lang,
			fmt.Sprintf("-metadata:s:a:%d", audioIndex), "title="+audio.Name,
		)
		inputIndex++
	}
	for subtitleIndex, subtitle := range selection.Subtitles {
		args = append(args,
			"-map", fmt.Sprintf("%d:s:0", inputIndex),
			fmt.Sprintf("-metadata:s:s:%d", subtitleIndex), "language="+subtitle.Lang,
			fmt.Sprintf("-metadata:s:s:%d", subtitleIndex), "title="+subtitle.Name,
		)
		inputIndex++
	}

	args = append(args, "-c", "copy")
	for subtitleIndex, subtitle := range selection.Subtitles {
		if isWebVTTSubtitle(subtitle) {
			// WebVTT is not a compatible subtitle codec for Matroska. Convert
			// only these streams to SubRip; leave ASS and other formats copied.
			args = append(args, fmt.Sprintf("-c:s:%d", subtitleIndex), "srt")
		}
	}
	if selection.Container == ContainerMP4 {
		args = append(args, "-movflags", "+faststart", "-f", "mp4")
	} else {
		args = append(args, "-f", "matroska")
	}
	if reportProgress {
		args = append(args, "-progress", "pipe:1", "-nostats")
	}
	return append(args, outputPath)
}

func isHLSQuality(quality models.Quality) bool {
	return strings.EqualFold(strings.TrimSpace(quality.Type), "hls")
}

func isWebVTTSubtitle(subtitle models.Subtitle) bool {
	return strings.EqualFold(strings.TrimSpace(subtitle.Type), "vtt") ||
		strings.EqualFold(strings.TrimSpace(subtitle.Type), "webvtt") ||
		strings.EqualFold(strings.TrimSpace(subtitle.Codec), "webvtt") ||
		strings.EqualFold(filepath.Ext(subtitle.OutputFile), ".vtt")
}

// qualityInputPath supports both historical HLS rows (Path includes
// master.m3u8) and the corrected convention (Path is the quality directory).
// HLS paths produced by the converter are relative to the permanent media root.
func qualityInputPath(root string, quality models.Quality) string {
	path := strings.TrimSpace(quality.Path)
	output := strings.TrimSpace(quality.OutputFile)
	if output == "" {
		return path
	}
	output = filepath.Base(output)
	if filepath.Base(path) == output {
		path = filepath.Dir(path)
	}
	if path == "" {
		return output
	}

	if isHLSQuality(quality) && !filepath.IsAbs(path) && strings.TrimSpace(root) != "" {
		rootAbs, rootErr := filepath.Abs(root)
		pathAbs, pathErr := filepath.Abs(path)
		alreadyUnderRoot := false
		if rootErr == nil && pathErr == nil {
			rel, err := filepath.Rel(rootAbs, pathAbs)
			alreadyUnderRoot = err == nil &&
				rel != ".." &&
				!strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		if !alreadyUnderRoot {
			path = filepath.Join(root, path)
		}
	}
	return filepath.Join(path, output)
}
