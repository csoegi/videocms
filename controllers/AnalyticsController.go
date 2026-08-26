package controllers

import (
	"net/http"
	"github.com/labstack/echo/v4"
)

// Log streaming events for analytics
func (h *Handlers) LogStreamEvent(c echo.Context) error {
	uuid := c.Param("UUID")
	h.Logic.RecordEvent("stream", uuid, c.RealIP(), c.Request().UserAgent())
	return c.NoContent(http.StatusOK)
}