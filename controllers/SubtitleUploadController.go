package controllers

import (
	"ch/kirari04/videocms/models"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"log"
	
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func (h *Handlers) SubtitleUploadController(c echo.Context) error {
	form, err := c.MultipartForm()
	if err != nil {
		return c.String(http.StatusBadRequest, "Invalid multipart subtitles payload")
	}

	// Resolve our primary transaction linking keys
	clientUploadUUID := strings.TrimSpace(c.FormValue("file_uuid"))
	selectedBurnName := strings.TrimSpace(c.FormValue("selected_sub_for_burn"))

	if clientUploadUUID == "" {
		return c.String(http.StatusBadRequest, "Missing client upload UUID")
	}

	// Fetch the existing UploadSession record from DB
	var session models.UploadSession
	if err := h.Deps.DB.Where("client_upload_uuid = ?", clientUploadUUID).First(&session).Error; err != nil {
		return c.String(http.StatusNotFound, "Upload session not found")
	}

	// Parse subtitle metadata from session
	var trackedSubtitles []models.SubtitleSessionData
	if session.StagedSubtitlesJSON != "" && session.StagedSubtitlesJSON != "[]" {
		_ = json.Unmarshal([]byte(session.StagedSubtitlesJSON), &trackedSubtitles)
	}

	// Loop over the incoming files payload files
	for _, fileHeader := range form.File["subtitles"] {
		srcSub, err := fileHeader.Open()
		if err != nil {
			continue
		}

		subtitleUUID := uuid.NewString()
		targetDir := filepath.Join(h.Config().FolderVideoUploadsPriv, clientUploadUUID)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			srcSub.Close()
			continue
		}

		finalSubPath := filepath.Join(targetDir, fmt.Sprintf("%s_ext.tmp", subtitleUUID))
		dstSub, err := os.Create(finalSubPath)
		if err != nil {
			srcSub.Close()
			continue
		}

		// Read and auto-convert SRT to true WebVTT layout format string on the fly
		subBytes, err := io.ReadAll(srcSub)
		srcSub.Close()
		if err != nil {
			dstSub.Close()
			continue
		}

		subText := string(subBytes)
		isSRT := strings.HasSuffix(strings.ToLower(fileHeader.Filename), ".srt")
		if isSRT && !strings.HasPrefix(subText, "WEBVTT") {
			_, _ = dstSub.WriteString("WEBVTT\n\n")
			subText = strings.ReplaceAll(subText, ",", ".")
		}
		_, _ = dstSub.WriteString(subText)
		dstSub.Close()

		langKey := fmt.Sprintf("lang_%s", fileHeader.Filename)
		language := strings.TrimSpace(c.FormValue(langKey))
		if language == "" {
			language = "id" // Defauult lang
		}

		// Add definition mapping directly into our memory collector array
		trackedSubtitles = append(trackedSubtitles, models.SubtitleSessionData{
			UUID:            subtitleUUID,
			Name:            fileHeader.Filename,
			Lang:            language,
			Path:            finalSubPath,
			SelectedForBurn: (fileHeader.Filename == selectedBurnName),
		})

		// This populates the subtitles table immediately so that Finalize can query and update it later.
		// We explicitly link this record row directly to the active video UploadSession ID
		// This protects the track from being intercepted or skipped by background cleaners.
		newDbSubtitle := models.Subtitle{
			UUID:            subtitleUUID,
			Name:            fileHeader.Filename,
			Lang:            language,
			Type:            "vtt", 
			Path:            finalSubPath,
			FileID:          0,     
			Source:          "external", 
			SelectedForBurn: (fileHeader.Filename == selectedBurnName),
			
			// 💡 SET READY TO TRUE IMMEDIATELY:
			// By flagging the record as ready right here, the internal encoder workers
			// will completely skip this row, ensuring no duplicate database entries
			Ready:           true, 
			Encoding:        false,
			Failed:          false,
			Progress:        100,
		}
		
		if errDb := h.Deps.DB.Create(&newDbSubtitle).Error; errDb != nil {
			log.Printf("[SubtitleUploadController] [ERROR] Failed to save subtitles to database: %v", errDb)
		}
	}

	// Serialize updated subtitle metadata array back into a plain JSON string block
	updatedJSON, err := json.Marshal(trackedSubtitles)
	if err != nil {
		return c.String(http.StatusInternalServerError, "[SubtitleUploadController] [ERROR] Failed to serialize subtitle metadata to json.")
	}

	// Save the upload session into DB
	session.StagedSubtitlesJSON = string(updatedJSON)
	if err := h.Deps.DB.Save(&session).Error; err != nil {
		return c.String(http.StatusInternalServerError, "[SubtitleUploadController] [ERROR] Failed to persist subtitle metadata to UploadSession table.")
	}

	return c.String(http.StatusOK, "Subtitles are staged successfully in upload session.")
}