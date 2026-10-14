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

	// ⚠️ Imported MP4 files are already the final media asset. Serve them directly with
	// net/http range support; do not invoke the HLS/audio-selection or FFmpeg pipeline.
	for _, quality := range dbLink.File.Qualitys {
		if quality.Name != requestValidation.QUALITY || quality.Type != "mp4" {
			continue
		}
		if !quality.Ready {
			return c.String(http.StatusConflict, "imported video is not ready")
		}
		claims, ok := middlewares.MediaClaims(c)
		if !ok || claims.QualityIDs[quality.Name] != quality.ID {
			return c.String(http.StatusForbidden, "media token does not authorize this quality")
		}
		filePath := filepath.Join(h.Config().PublishedQualitysRoot(), dbLink.File.UUID, quality.Name, quality.OutputFile)
		file, err := os.Open(filePath)
		if err != nil {
			return c.String(http.StatusNotFound, "imported video file not found")
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return c.String(http.StatusNotFound, "imported video file not found")
		}
		h.Logic.TrackTraffic(dbLink.File.UserID, dbLink.FileID, quality.ID, 0, uint64(info.Size()))
		h.Logic.RecordEvent("download", requestValidation.UUID, c.RealIP(), c.Request().UserAgent())
		c.Response().Header().Set("Accept-Ranges", "bytes")
		c.Response().Header().Set("Content-Type", "video/mp4")
		fileName := fmt.Sprintf("%s[%s].mp4", safeDownloadName(dbLink.Name), requestValidation.QUALITY)
		if c.QueryParam("download") == "1" {
			c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
		}
		http.ServeContent(c.Response(), c.Request(), fileName, info.ModTime(), file)
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
