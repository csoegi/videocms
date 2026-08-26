package controllers

import (
	"ch/kirari04/videocms/helpers"
	"ch/kirari04/videocms/models"
	"net/http"
	"github.com/labstack/echo/v4"
)

// List all ads for the authenticated user
// FIX 1: Attach to the (h *Handlers) struct receiver
func (h *Handlers) AdvertisementList(c echo.Context) error {
	var ads []models.Advertisement
	
	val := c.Get("UserID")
	if val == nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{"error": "User context missing"})
	}
	userID := val.(uint)

	// FIX 2: Swap out inits.DB for h.Deps.DB
	if err := h.Deps.DB.Where("user_id = ?", userID).Order("ads_priority desc").Find(&ads).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": "Database query failed"})
	}

	return c.JSON(http.StatusOK, ads)
}

// Create a new advertisement
// FIX 1: Attach to the (h *Handlers) struct receiver
func (h *Handlers) AdvertisementCreate(c echo.Context) error {
	var input models.AdvertisementCreateValidation
	
	// Validate using your project's helper
	if status, err := helpers.Validate(c, &input); err != nil {
		return c.JSON(status, echo.Map{"error": err.Error()})
	}

	val := c.Get("UserID")
	if val == nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{"error": "Unauthorized"})
	}
	userID := val.(uint)

	ad := models.Advertisement{
		UserID:      userID,
		AdsName:     input.AdsName,
		AdsType:     input.AdsType,
		AdsLink:     input.AdsLink,
		AdsDisplay:  input.AdsDisplay,
		AdsDevice:   input.AdsDevice,
		AdsPriority: input.AdsPriority,
		IsActive:    true,
	}

	// FIX 2: Swap out inits.DB for h.Deps.DB
	if err := h.Deps.DB.Create(&ad).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": "Database error"})
	}

	return c.JSON(http.StatusCreated, ad)
}

// Update an existing ad
// FIX 1: Attach to the (h *Handlers) struct receiver
func (h *Handlers) AdvertisementUpdate(c echo.Context) error {
	id := c.Param("id")
	val := c.Get("UserID")
	if val == nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{"error": "Unauthorized"})
	}
	userID := val.(uint)

	// 1. Find the ad first to ensure it exists and belongs to the user
	var ad models.Advertisement
	// FIX 2: Swap out inits.DB for h.Deps.DB
	if err := h.Deps.DB.Where("id = ? AND user_id = ?", id, userID).First(&ad).Error; err != nil {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "Ad not found"})
	}

	// 2. Use helper to Bind + Validate in ONE step
	var input models.AdvertisementUpdateValidation
	if status, err := helpers.Validate(c, &input); err != nil {
		return c.JSON(status, echo.Map{"error": err.Error()})
	}

	// 3. Perform the update
	// GORM will ignore nil pointers in the input struct due to Updates() logic
	// FIX 2: Swap out inits.DB for h.Deps.DB
	if err := h.Deps.DB.Model(&ad).Updates(input).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": "Update failed"})
	}

	return c.JSON(http.StatusOK, ad)
}

// Delete an advertisement
// FIX 1: Attach to the (h *Handlers) struct receiver
func (h *Handlers) AdvertisementDelete(c echo.Context) error {
	id := c.Param("id")
	val := c.Get("UserID")
	if val == nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{"error": "Unauthorized"})
	}
	userID := val.(uint)

	// FIX 2: Swap out inits.DB for h.Deps.DB
	if err := h.Deps.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&models.Advertisement{}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": "Delete failed"})
	}

	return c.NoContent(http.StatusNoContent)
}
