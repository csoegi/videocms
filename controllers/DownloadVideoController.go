package controllers

import (
	downloadsvc "ch/kirari04/videocms/download"
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/middlewares"
	"ch/kirari04/videocms/models"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

const (
	downloadContainerMKV = downloadsvc.ContainerMKV
	downloadContainerMP4 = downloadsvc.ContainerMP4
)

var downloadQualityPattern = regexp.MustCompile(`^([0-9]{3,4}p|(h264))$`)

type resolvedDownloadSelection = downloadsvc.Selection

func (h *Handlers) DownloadVideoController(c echo.Context) error {
	type Request struct {
		UUID    string `validate:"required,uuid_rfc4122" param:"UUID"`
		QUALITY string `validate:"required,min=1,max=10" param:"QUALITY"`
		Stream  *bool  `validate:"omitempty,boolean" param:"STREAM"`
	}
	var requestValidation Request
	if status, err := helpers.Validate(c, &requestValidation); err != nil {
		return c.String(status, err.Error())
	}

	if !downloadQualityPattern.MatchString(requestValidation.QUALITY) {
		return c.String(http.StatusBadRequest, "bad quality format")
	}
	if !h.downloadsEnabled() {
		return c.String(http.StatusBadRequest, "download disabled")
	}

	if requestValidation.Stream == nil || !*requestValidation.Stream {
		return c.String(http.StatusBadRequest, "unsupported progressive stream")
	}

	var dbLink models.Link
	if dbRes := h.Deps.DB.
		Preload("File").
		Preload("File.Subtitles").
		Preload("File.Audios").
		Preload("File.Qualitys").
		Where(&models.Link{UUID: requestValidation.UUID}).
		First(&dbLink); dbRes.Error != nil {
		return c.String(http.StatusBadRequest, "video doesn't exist")
	}

	// The route suffix determines the requested output container. Direct media
	// can be served as-is when source and target match; otherwise use a
	// stream-copy remux, optionally adding user-selected sidecar subtitles.
	targetContainer := strings.TrimPrefix(strings.ToLower(filepath.Ext(c.Request().URL.Path)), ".")
	if targetContainer != "mp4" && targetContainer != "mkv" {
		return c.String(http.StatusBadRequest, "unsupported output container")
	}
	requestedSubtitleUUIDs := requestedSubtitleIDs(c)

	for _, quality := range dbLink.File.Qualitys {
		qualityType := strings.ToLower(quality.Type)
		if quality.Name != requestValidation.QUALITY || (qualityType != "mp4" && qualityType != "mkv") {
			continue
		}
		if !quality.Ready {
			return c.String(http.StatusConflict, "imported video is not ready")
		}
		claims, ok := middlewares.MediaClaims(c)
		if !ok || claims.QualityIDs[quality.Name] != quality.ID {
			return c.String(http.StatusForbidden, "media token does not authorize this quality")
		}
		filePath := filepath.Join(h.Config().PublishedQualitysRoot(), dbLink.File.UUID, filepath.Base(quality.Name), filepath.Base(quality.OutputFile))
		info, statErr := os.Stat(filePath)
		if statErr != nil || !info.Mode().IsRegular() {
			return c.String(http.StatusNotFound, "imported video file not found")
		}

		selectedSubtitles, err := resolveDirectDownloadSubtitles(dbLink.File.Subtitles, dbLink.FileID, requestedSubtitleUUIDs)
		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}
		if targetContainer == "mp4" && len(selectedSubtitles) > 0 {
			return c.String(http.StatusBadRequest, "external subtitles can only be included in MKV downloads")
		}

		fileExtension := "." + targetContainer
		fileName := fmt.Sprintf("%s[%s]%s", safeDownloadName(dbLink.Name), requestValidation.QUALITY, fileExtension)
		contentType := "video/mp4"
		if targetContainer == "mkv" {
			contentType = "video/x-matroska"
		}

		if targetContainer == qualityType && len(selectedSubtitles) == 0 {
			file, err := os.Open(filePath)
			if err != nil {
				return c.String(http.StatusNotFound, "imported video file not found")
			}
			defer file.Close()
			if c.QueryParam("download") == "1" {
				c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
			}
			c.Response().Header().Set("Content-Type", contentType)
			c.Response().Header().Set("Accept-Ranges", "bytes")
			h.Logic.TrackTraffic(dbLink.File.UserID, dbLink.FileID, quality.ID, 0, uint64(info.Size()))
			h.Logic.RecordEvent("download", requestValidation.UUID, c.RealIP(), c.Request().UserAgent())
			http.ServeContent(c.Response(), c.Request(), fileName, info.ModTime(), file)
			return nil
		}

		tmpPath := filepath.Join(h.Config().FolderVideoUploadsPriv, fmt.Sprintf("%s-download.%s", uuid.NewString(), targetContainer))
		defer os.Remove(tmpPath)
		if err := os.MkdirAll(filepath.Dir(tmpPath), 0755); err != nil {
			return c.String(http.StatusInternalServerError, "failed to prepare converted download")
		}
		if err := remuxDirectMediaDownload(c.Request().Context(), filePath, tmpPath, targetContainer, selectedSubtitles); err != nil {
			if errors.Is(c.Request().Context().Err(), context.Canceled) {
				return nil
			}
			c.Logger().Errorf("Failed to remux direct media to %s: %v", targetContainer, err)
			return c.String(http.StatusUnprocessableEntity, fmt.Sprintf("cannot convert this video to %s without re-encoding: %v", strings.ToUpper(targetContainer), err))
		}
		file, err := os.Open(tmpPath)
		if err != nil {
			return c.String(http.StatusInternalServerError, "failed to open converted video")
		}
		defer file.Close()
		convertedInfo, err := file.Stat()
		if err != nil || !convertedInfo.Mode().IsRegular() {
			return c.String(http.StatusInternalServerError, "failed to inspect converted video")
		}
		if c.QueryParam("download") == "1" {
			c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
		}
		c.Response().Header().Set("Content-Type", contentType)
		c.Response().Header().Set("Accept-Ranges", "bytes")
		h.Logic.TrackTraffic(dbLink.File.UserID, dbLink.FileID, quality.ID, 0, uint64(convertedInfo.Size()))
		h.Logic.RecordEvent("download", requestValidation.UUID, c.RealIP(), c.Request().UserAgent())
		http.ServeContent(c.Response(), c.Request(), fileName, convertedInfo.ModTime(), file)
		return nil
	}

	selection, err := resolveDownloadSelection(
		&dbLink,
		requestValidation.QUALITY,
		downloadContainerMP4,
		true,
		false,
		nil,
		nil,
	)
	if err != nil {
		return c.String(http.StatusBadRequest, err.Error())
	}
	selection.QualityRoot = h.Config().PublishedQualitysRoot()

	tmpFilePath := filepath.Join(
		h.Config().FolderVideoUploadsPriv,
		fmt.Sprintf("%s-download.%s", uuid.NewString(), selection.Container),
	)
	defer os.Remove(tmpFilePath)

	cmdArgs := downloadFFmpegArgs(selection, tmpFilePath)
	cmd := exec.CommandContext(c.Request().Context(), "ffmpeg", cmdArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(c.Request().Context().Err(), context.Canceled) {
			return nil
		}
		c.Logger().Errorf("Failed to assemble download: %v: %s", err, strings.TrimSpace(string(output)))
		return c.String(http.StatusInternalServerError, "failed to prepare download")
	}

	tmpFile, err := os.Open(tmpFilePath)
	if err != nil {
		c.Logger().Error("Failed to open assembled download", err)
		return c.String(http.StatusInternalServerError, "failed to open prepared download")
	}
	defer tmpFile.Close()

	fileInfo, err := tmpFile.Stat()
	if err != nil {
		return c.String(http.StatusInternalServerError, "failed to inspect prepared download")
	}
	h.Logic.TrackTraffic(
		dbLink.File.UserID,
		dbLink.FileID,
		selection.Quality.ID,
		0,
		uint64(fileInfo.Size()),
	)
	// Record the "download" event with all details for analytics (non-blocking, fire-and-forget)
	h.Logic.RecordEvent("download", requestValidation.UUID, c.RealIP(), c.Request().UserAgent())

	fileName := fmt.Sprintf("%s[%s].mp4", safeDownloadName(dbLink.Name), requestValidation.QUALITY)
	c.Response().Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(c.Response(), c.Request(), fileName, time.Now(), tmpFile)
	return nil
}

func resolveDownloadSelection(
	dbLink *models.Link,
	qualityName string,
	container string,
	streaming bool,
	customSelection bool,
	audioUUIDs []string,
	subtitleUUIDs []string,
) (*resolvedDownloadSelection, error) {
	return downloadsvc.ResolveSelection(
		dbLink,
		qualityName,
		container,
		streaming,
		customSelection,
		audioUUIDs,
		subtitleUUIDs,
	)
}

func downloadFFmpegArgs(selection *resolvedDownloadSelection, outputPath string) []string {
	return downloadsvc.FFmpegArgs(selection, outputPath, false)
}

func safeDownloadName(name string) string {
	safe := regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(name, "-")
	safe = strings.Trim(safe, "-")
	if safe == "" {
		return "video"
	}
	return safe
}

func requestedSubtitleIDs(c echo.Context) []string {
	values := append([]string{}, c.QueryParams()["subtitleUUID"]...)
	values = append(values, c.QueryParams()["subtitleUUIDs"]...)
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
	for _, value := range values {
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

func resolveDirectDownloadSubtitles(all []models.Subtitle, fileID uint, requested []string) ([]models.Subtitle, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	byUUID := make(map[string]models.Subtitle, len(all))
	for _, sub := range all {
		if sub.FileID == fileID && sub.Ready && !sub.Failed {
			byUUID[sub.UUID] = sub
		}
	}
	resolved := make([]models.Subtitle, 0, len(requested))
	for _, id := range requested {
		sub, ok := byUUID[id]
		if !ok {
			return nil, fmt.Errorf("subtitle %s is not a ready subtitle for this video", id)
		}
		if sub.Path == "" || filepath.Base(sub.OutputFile) == "." || filepath.Base(sub.OutputFile) == "" {
			return nil, fmt.Errorf("subtitle %s has an invalid media path", id)
		}
		resolved = append(resolved, sub)
	}
	return resolved, nil
}

func remuxDirectMediaDownload(ctx context.Context, sourcePath, outputPath, container string, subtitles []models.Subtitle) error {
	if container != "mp4" && container != "mkv" {
		return fmt.Errorf("unsupported output container %q", container)
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", sourcePath}
	for _, sub := range subtitles {
		subPath := filepath.Join(sub.Path, filepath.Base(sub.OutputFile))
		if info, err := os.Stat(subPath); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("subtitle file is unavailable: %s", sub.Name)
		}
		if strings.EqualFold(sub.Type, "vtt") ||
			strings.EqualFold(sub.Type, "webvtt") ||
			strings.EqualFold(sub.Codec, "webvtt") ||
			strings.EqualFold(filepath.Ext(sub.OutputFile), ".vtt") {
			args = append(args, "-f", "webvtt")
		}
		args = append(args, "-i", subPath)
	}
	args = append(args, "-map", "0:v:0")
	if container == "mp4" {
		// The download UI intentionally limits MP4 to a single audio track.
		args = append(args, "-map", "0:a:0?")
	} else {
		args = append(args, "-map", "0:a?")
	}
	args = append(args, "-map_metadata", "0", "-map_chapters", "0", "-c", "copy")
	if len(subtitles) > 0 {
		if container == "mp4" {
			return errors.New("external subtitles are not supported in MP4 output")
		}
		for i := range subtitles {
			args = append(args, "-map", fmt.Sprintf("%d:0", i+1))
		}
		args = append(args, "-c:s", "srt")
		for i, sub := range subtitles {
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "language="+safeFFmpegMetadata(sub.Lang))
			args = append(args, fmt.Sprintf("-metadata:s:s:%d", i), "title="+safeFFmpegMetadata(sub.Name))
			if i == 0 {
				args = append(args, "-disposition:s:0", "default")
			}
		}
	}
	if container == "mp4" {
		args = append(args, "-movflags", "+faststart", "-f", "mp4", outputPath)
	} else {
		args = append(args, "-f", "matroska", outputPath)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg remux failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	info, err := os.Stat(outputPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("ffmpeg did not produce a valid output file")
	}
	return nil
}

func safeFFmpegMetadata(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}
