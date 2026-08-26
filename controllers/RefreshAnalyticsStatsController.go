package controllers

import (
	"net/http"
	"github.com/labstack/echo/v4"
)

// FIX 1: Attach to the (h *Handlers) struct receiver
func (h *Handlers) RefreshAnalyticsStats(c echo.Context) error {
	
	if !h.Logic.Config().AnalyticsEnabled {
		return c.JSON(http.StatusForbidden, echo.Map{"error": "Disabled"})
	}
	
	h.Logic.BuildAnalyticsReport()
	
	return c.JSON(http.StatusOK, echo.Map{"message": "Report refreshed"})
}
