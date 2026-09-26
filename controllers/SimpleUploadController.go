package controllers

import (
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/models"
	"fmt"
	"net/http"
	"os"
	"io"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func (h *Handlers) SimpleUploadController(c echo.Context) error {
	// check if uploads are enabled
	if !*h.Config().UploadEnabled {
		return c.String(http.StatusForbidden, "Uploads are disabled")
	}

	userID, ok := c.Get("UserID").(uint)
	if !ok {
		return c.String(http.StatusInternalServerError, "Failed to catch UserID")
	}

	// parse & validate request
	var validation models.SimpleUploadValidation
	if status, err := helpers.Validate(c, &validation); err != nil {
		return c.String(status, err.Error())
	}

	// file processing
	file, err := c.FormFile("file")
	if err != nil {
		return c.String(http.StatusBadRequest, "No file uploaded")
	}

	// size check
	if file.Size > h.Config().MaxUploadFilesize {
		return c.String(http.StatusRequestEntityTooLarge, fmt.Sprintf("Exceeded max upload filesize: %v", h.Config().MaxUploadFilesize))
	}

	src, err := file.Open()
	if err != nil {
		c.Logger().Error("Failed to open uploaded file", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	defer src.Close()

	// 🌟 Initialize Subtitles From Upload Request
	var externalSubs []models.Subtitle
	form, err := c.MultipartForm()
	
	if err == nil {
		subtitleFiles := form.File["subtitles"]
		for i, subHeader := range subtitleFiles {
			subSrc, errSub := subHeader.Open()
			if errSub != nil {
				continue
			}

			// Capture language tags sent by the frontend form data (e.g., lang_english.srt)
			langKey := fmt.Sprintf("lang_%s", subHeader.Filename)
			subLang := c.FormValue(langKey)
			if subLang == "" {
				subLang = "und"
			}

			// Generate unified staging identification parameters
			subUUID := uuid.NewString()
			tempSubPath := fmt.Sprintf("%s/%s_simplesub_%d.tmp", h.Config().FolderVideoUploadsPriv, subUUID, i)
			
			dstSub, errSub := os.Create(tempSubPath)
			if errSub != nil {
				subSrc.Close()
				continue
			}

			// Consume multipart form stream bytes directly inside the controller
			subBytes, errSub := io.ReadAll(subSrc)
			subSrc.Close()
			if errSub != nil {
				dstSub.Close()
				_ = os.Remove(tempSubPath)
				continue
			}

			// 🌟 AUTOMATED FORMAT CONVERSION: Auto convert SRT to player-safe WebVTT format on the fly
			subText := string(subBytes)
			isSRT := strings.HasSuffix(strings.ToLower(subHeader.Filename), ".srt")
			if isSRT && !strings.HasPrefix(subText, "WEBVTT") {
				_, _ = dstSub.WriteString("WEBVTT\n\n")
				subText = strings.ReplaceAll(subText, ",", ".")
			}
			_, _ = dstSub.WriteString(subText)
			dstSub.Close()

			// Check if this file name matches what the user selected for a hard burn
			shouldBurn := (subHeader.Filename == validation.SelectedSubForBurn)

			// Populate model properties directly into the collection array
			externalSubs = append(externalSubs, models.Subtitle{
				UUID:            subUUID,
				Name:            subHeader.Filename,
				Lang:            subLang,
				Path:            tempSubPath, // Relays temp location straight down to CreateFile
				Source:          "external",
				SelectedForBurn: shouldBurn,
				Ready:           false, // Transcoder engine will unlock this once video settles
				Encoding:        false,
				Failed:          false,
				Progress:        0.0,
				Index:           -1, // Bypasses background extractions cleanly
			})
		}
	}

	// Wipes any successfully streamed temporary subtitles out of disk if the transaction aborts!
	defer func() {
		if err != nil {
			for _, sub := range externalSubs {
				if sub.Path != "" {
					_ = os.Remove(sub.Path)
				}
			}
		}
	}()

	// Business logic (Passes down clean native models array)
	status, response, err := h.Logic.SimpleUpload(
		validation.ParentFolderID,
		validation.Name,
		src,
		file.Size,
		userID,
		validation.SubtitleMode, 
		externalSubs,
	)
	if err != nil {
		return c.String(status, err.Error())
	}

	return c.JSON(status, response)
}

