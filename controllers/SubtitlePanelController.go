package controllers

import (
	"ch/kirari04/videocms/models"
	"ch/kirari04/videocms/helpers"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// Upload Subtitle for a video
func (h *Handlers) AddSubtitleTrackDirect(c echo.Context) error {
	videoUUID := c.Param("uuid")

	language := strings.TrimSpace(c.FormValue("lang"))
	if language == "" {
		language = "id" // Defauult lang
	}

	var file models.File
	if err := h.Deps.DB.Where("uuid = ?", videoUUID).First(&file).Error; err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Video record not found"})
	}

	formFile, err := c.FormFile("subtitle_file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No file uploaded"})
	}

	src, err := formFile.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to open track"})
	}
	defer src.Close()

	subtitleUUID := uuid.NewString()
	productionSubDir := filepath.Join(h.Config().FolderVideoQualitysPriv, file.UUID, subtitleUUID)
	targetOutputName := fmt.Sprintf("%s.srt", subtitleUUID)
	finalDiskFilePath := filepath.Join(productionSubDir, targetOutputName)

	if errMk := os.MkdirAll(productionSubDir, 0755); errMk != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create directory"})
	}

	dst, err := os.Create(finalDiskFilePath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to write file"})
	}

	subBytes, _ := io.ReadAll(src)
	subText := string(subBytes)
	isSRT := strings.HasSuffix(strings.ToLower(formFile.Filename), ".srt")
	if isSRT && !strings.HasPrefix(subText, "WEBVTT") {
		_, _ = dst.WriteString("WEBVTT\n\n")
		subText = strings.ReplaceAll(subText, ",", ".")
	}
	_, _ = dst.WriteString(subText)
	dst.Close()

	newSub := models.Subtitle{
		UUID:            subtitleUUID,
		Name:            formFile.Filename,
		Lang:            language,
		Type:            "vtt",
		Path:            productionSubDir,
		OutputFile:      targetOutputName,
		FileID:          file.ID,
		Source:          "external",
		Index:           -1, // 💡 HARDCODED TO -1 TO INSULATE FROM WORKERS
		Ready:           true,
		Progress:        100.0,
	}

	if errDb := h.Deps.DB.Create(&newSub).Error; errDb != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Database write failed"})
	}

	return c.JSON(http.StatusOK, newSub)
}

// Update SubtitleMode ("none", "soft", "hard") and hard-burn track selection
func (h *Handlers) UpdateVideoSubtitleConfig(c echo.Context) error {
	videoUUID := c.Param("uuid")
	
	var payload models.SubtitleSettingValidation
	if status, err := helpers.Validate(c, &payload); err != nil {
		return c.String(status, err.Error())
	}

	var file models.File
	if err := h.Deps.DB.Preload("Qualitys").Where("uuid = ?", videoUUID).First(&file).Error; err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Video record not found"})
	}

	// Track if a hard-burn change occurred requiring a re-encode run
	var triggerReEncode bool = false
	if file.SubtitleMode != payload.SubtitleMode && payload.SubtitleMode == "hard" {
		triggerReEncode = true
	}

	errTx := h.Deps.DB.Transaction(func(tx *gorm.DB) error {
		// 1. Update the master subtitle mode parameter
		if err := tx.Model(&file).Update("subtitle_mode", payload.SubtitleMode).Error; err != nil {
			return err
		}

		// 2. Clear old burn selections rows flags
		if err := tx.Model(&models.Subtitle{}).Where("file_id = ?", file.ID).Update("selected_for_burn", false).Error; err != nil {
			return err
		}

		// 3. Mark the new selected track for burning
		if payload.SelectedBurnUUID != "" {
			var targetSub models.Subtitle
			if errSub := tx.Model(&models.Subtitle{}).Where("file_id = ? AND uuid = ?", file.ID, payload.SelectedBurnUUID).First(&targetSub).Error; errSub == nil {
				if !targetSub.SelectedForBurn {
					triggerReEncode = true // Re-encode triggered if a brand new track is chosen!
				}
				tx.Model(&targetSub).Update("selected_for_burn", true)
			}
		}

		// 4. If a hard-burn selected, drop the resolution qualities back to pending so encoder will pick it up
		if triggerReEncode {
			for _, quality := range file.Qualitys {
				// 1. Cleanly wipe old physical directory
				if quality.Path != "" && strings.Contains(quality.Path, "qualitys") {
					_ = os.RemoveAll(quality.Path)
				}
				
				// 2. Reset Video Quality Status so Encoder worker will pick it up for re-encoding
				tx.Model(&models.Quality{}).Where("id = ?", quality.ID).Updates(map[string]interface{}{
					"ready":    false,
					"failed":   false,
					"encoding": false,
					"progress": 0.0,
					"error":    "",
				})
			}
		}

		return nil
	})

	if errTx != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": errTx.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Configuration saved. Transcoding re-queued if needed."})
}

// Delete subtitle from DB and its folder from hard drive
func (h *Handlers) DeleteSubtitleTrackDirect(c echo.Context) error {
	subUUID := c.Param("sub_uuid") 

	var sub models.Subtitle
	if err := h.Deps.DB.Where("uuid = ?", subUUID).First(&sub).Error; err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Subtitle not found in DB"})
	}

	if sub.Path != "" && strings.Contains(sub.Path, "qualitys") {
		_ = os.RemoveAll(sub.Path)
	}

	h.Deps.DB.Delete(&sub)
	return c.JSON(http.StatusOK, map[string]string{"message": "Subtitle successfully purged"})
}
