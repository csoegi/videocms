package controllers

import (
	"ch/kirari04/videocms/models"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

var (
	srtTimestampPattern = regexp.MustCompile(`(?m)^\s*\d{2,}:\d{2}:\d{2},\d{3}\s+-->\s+\d{2,}:\d{2}:\d{2},\d{3}`)
	srtTimestampComma   = regexp.MustCompile(`(\d{2,}:\d{2}:\d{2}),(\d{3})`)
)

type validatedSubtitleUpload struct {
	filename string
	language string
	content  string
	uuid     string
	path     string
	selected bool
}

func validateSubtitleFilename(filename string) (string, string, error) {
	filename = strings.TrimSpace(filename)
	if filename == "" || strings.ContainsAny(filename, "/\\") {
		return "", "", fmt.Errorf("invalid subtitle filename")
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return "", "", fmt.Errorf("invalid subtitle filename")
		}
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".srt" && ext != ".vtt" {
		return "", "", fmt.Errorf("unsupported subtitle file type: %s (only .srt and .vtt are accepted)", ext)
	}
	return filename, ext, nil
}

func validateAndConvertSubtitle(filename, ext string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%s is empty", filename)
	}

	content := strings.TrimPrefix(string(data), "\uFEFF")
	if ext == ".vtt" {
		header := strings.TrimSpace(content)
		if !strings.HasPrefix(header, "WEBVTT") {
			return "", fmt.Errorf("%s does not have a valid WebVTT header", filename)
		}
		if len(header) > len("WEBVTT") {
			next := header[len("WEBVTT")]
			if next != ' ' && next != '\t' && next != '\r' && next != '\n' {
				return "", fmt.Errorf("%s does not have a valid WebVTT header", filename)
			}
		}
		return content, nil
	}

	if !srtTimestampPattern.MatchString(content) {
		return "", fmt.Errorf("%s does not contain a valid SRT subtitle cue", filename)
	}

	// Convert only timestamp millisecond separators; preserve commas in subtitle text.
	content = srtTimestampComma.ReplaceAllString(content, "$1.$2")
	return "WEBVTT\n\n" + content, nil
}

func (h *Handlers) SubtitleUploadController(c echo.Context) error {
	form, err := c.MultipartForm()
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid multipart subtitles payload")
	}
	defer form.RemoveAll()

	clientUploadUUID := strings.TrimSpace(c.FormValue("file_uuid"))
	selectedBurnName := strings.TrimSpace(c.FormValue("selected_sub_for_burn"))

	if _, err := uuid.Parse(clientUploadUUID); err != nil {
		return c.String(http.StatusBadRequest, "Invalid client upload UUID")
	}

	files := form.File["subtitles"]
	if len(files) == 0 {
		return c.String(http.StatusBadRequest, "No subtitle files were provided")
	}

	var session models.UploadSession
	if err := h.Deps.DB.Where("client_upload_uuid = ?", clientUploadUUID).First(&session).Error; err != nil {
		return c.String(http.StatusNotFound, "Upload session not found")
	}

	var trackedSubtitles []models.SubtitleSessionData
	if session.StagedSubtitlesJSON != "" && session.StagedSubtitlesJSON != "[]" {
		if err := json.Unmarshal([]byte(session.StagedSubtitlesJSON), &trackedSubtitles); err != nil {
			return c.String(http.StatusInternalServerError, "Invalid subtitle metadata in upload session")
		}
	}

	// Validate every file before writing anything, avoiding partially staged batches
	// when one of the selected files is not a supported subtitle.
	uploads := make([]validatedSubtitleUpload, 0, len(files))
	for _, fileHeader := range files {
		filename, ext, err := validateSubtitleFilename(fileHeader.Filename)
		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}

		src, err := fileHeader.Open()
		if err != nil {
			return c.String(http.StatusBadRequest, fmt.Sprintf("Unable to read subtitle %s", filename))
		}
		data, readErr := io.ReadAll(src)
		closeErr := src.Close()
		if readErr != nil || closeErr != nil {
			return c.String(http.StatusBadRequest, fmt.Sprintf("Unable to read subtitle %s", filename))
		}

		content, err := validateAndConvertSubtitle(filename, ext, data)
		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}

		langKey := fmt.Sprintf("lang_%s", fileHeader.Filename)
		language := strings.TrimSpace(c.FormValue(langKey))
		if language == "" {
			language = "id"
		}

		uploads = append(uploads, validatedSubtitleUpload{
			filename: filename,
			language: language,
			content:  content,
			uuid:     uuid.NewString(),
			selected: filename == selectedBurnName,
		})
	}

	targetDir := filepath.Join(h.Config().FolderVideoUploadsPriv, clientUploadUUID)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		log.Printf("[SubtitleUploadController] Failed to create subtitle staging directory: %v", err)
		return c.String(http.StatusInternalServerError, "Unable to create subtitle staging directory")
	}

	createdPaths := make([]string, 0, len(uploads))
	cleanupFiles := func() {
		for _, path := range createdPaths {
			_ = os.Remove(path)
		}
	}

	for i := range uploads {
		uploads[i].path = filepath.Join(targetDir, fmt.Sprintf("%s_ext.tmp", uploads[i].uuid))
		if err := os.WriteFile(uploads[i].path, []byte(uploads[i].content), 0644); err != nil {
			cleanupFiles()
			log.Printf("[SubtitleUploadController] Failed to stage subtitle %s: %v", uploads[i].filename, err)
			return c.String(http.StatusInternalServerError, "Failed to stage subtitle files")
		}
		createdPaths = append(createdPaths, uploads[i].path)
	}

	updatedSubtitles := append([]models.SubtitleSessionData(nil), trackedSubtitles...)
	for _, upload := range uploads {
		updatedSubtitles = append(updatedSubtitles, models.SubtitleSessionData{
			UUID:            upload.uuid,
			Name:            upload.filename,
			Lang:            upload.language,
			Path:            upload.path,
			SelectedForBurn: upload.selected,
		})
	}

	updatedJSON, err := json.Marshal(updatedSubtitles)
	if err != nil {
		cleanupFiles()
		return c.String(http.StatusInternalServerError, "Failed to serialize subtitle metadata")
	}

	// Keep subtitle rows and upload-session metadata in sync. If the transaction
	// fails, remove the newly staged files as well.
	err = h.Deps.DB.Transaction(func(tx *gorm.DB) error {
		for _, upload := range uploads {
			subtitle := models.Subtitle{
				UUID:            upload.uuid,
				Name:            upload.filename,
				Lang:            upload.language,
				Type:            "vtt",
				Path:            upload.path,
				FileID:          0,
				Source:          "external",
				SelectedForBurn: upload.selected,
				Ready:           true,
				Encoding:        false,
				Failed:          false,
				Progress:        100,
			}
			if err := tx.Create(&subtitle).Error; err != nil {
				return err
			}
		}

		return tx.Model(&models.UploadSession{}).
			Where("client_upload_uuid = ?", clientUploadUUID).
			Update("staged_subtitles_json", string(updatedJSON)).Error
	})
	if err != nil {
		cleanupFiles()
		log.Printf("[SubtitleUploadController] Failed to persist subtitle upload: %v", err)
		return c.String(http.StatusInternalServerError, "Failed to save subtitle metadata")
	}

	return c.String(http.StatusOK, "Subtitles are staged successfully in upload session.")
}
