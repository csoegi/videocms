package controllers

import (
	"ch/kirari04/videocms/logic" // Correctly imported
	"net/http"
	"github.com/labstack/echo/v4"
)

func (h *Handlers) GetAnalyticsStats(c echo.Context) error {
    if !h.Logic.Config().AnalyticsEnabled {
        return c.JSON(http.StatusForbidden, echo.Map{"error": "Analytics disabled"})
    }
    
    // FIX 1: Change h.Logic.CachedReport to logic.CachedReport
    if logic.CachedReport == nil {
        return c.JSON(http.StatusAccepted, echo.Map{"message": "Report generating..."})
    }

    // FIX 2: Change h.Logic.CachedReport to logic.CachedReport
    return c.JSON(http.StatusOK, logic.CachedReport)
}
