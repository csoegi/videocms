package controllers

import (
	"ch/kirari04/videocms/configdb"
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/models"
	"log"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

func (h *Handlers) UpdateSettings(c echo.Context) error {

	// parse & validate request
	var validation models.SettingValidation
	if status, err := helpers.Validate(c, &validation); err != nil {
		return c.String(status, err.Error())
	}

	// 1. Fetch previous settings using the validation ID
	var previousSetting models.Setting
	if res := h.Deps.DB.First(&previousSetting, validation.ID); res.Error != nil {
		return c.String(http.StatusBadRequest, "Setting not found by id")
	}
	
	// Track old states for worker lifecycle hooks
	remoteDownloadsWereEnabled := settingFlagEnabled(previousSetting.RemoteDownloadEnabled, true)
	remoteDownloadsNowDisabled := !settingFlagEnabled(validation.RemoteDownloadEnabled, true)
	downloadsWereEnabled := settingFlagEnabled(previousSetting.DownloadEnabled, true)
	downloadsNowDisabled := !settingFlagEnabled(validation.DownloadEnabled, true)

	// 2. SAFE UPDATE OPTION (Single-line mapping via structural embedding)
	// This replaces the 80+ lines of manual assignments while preserving metadata
	idBeforeMapping := previousSetting.ID
	createdAtBeforeMapping := previousSetting.CreatedAt // If your models.Model tracks CreatedAt
	
	previousSetting = validation.Setting // Overwrites matching payload fields cleanly
	
	previousSetting.ID = idBeforeMapping // Keep the correct tracking ID
	previousSetting.CreatedAt = createdAtBeforeMapping // Protect the original creation date

	// 3. Save the modified original record
	if res := h.Deps.DB.Save(&previousSetting); res.Error != nil {
		log.Println("Failed to save settings", res.Error)
		return c.NoContent(http.StatusInternalServerError)
	}
	
	snapshot, err := configdb.LoadSnapshot(h.Deps.DB, h.Config())
	if err != nil {
		log.Println("Failed to reload settings", err)
		return c.NoContent(http.StatusInternalServerError)
	}
	h.Deps.Snapshots.Replace(snapshot)
	
	if h.Workers != nil && remoteDownloadsWereEnabled && remoteDownloadsNowDisabled {
		h.Workers.CancelAllRemoteDownloads("Remote downloads disabled by administrator")
	}
	if h.Workers != nil && downloadsWereEnabled && downloadsNowDisabled {
		h.Workers.CancelAllDownloadPreparations("Downloads disabled by administrator")
	}
	
	return c.String(http.StatusOK, "ok")
}

func settingFlagEnabled(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1":
		return true
	case "false", "0":
		return false
	default:
		return fallback
	}
}
