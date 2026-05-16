package ui

import (
	"net/http"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const passcodeLog = "[ui-passcode] "

// listPasscodeResets returns pending passcode (PIN) reset requests
// initiated by the caller's devices. Requests are dropped lazily once
// they exceed passcodestore.ResetTTL (24h).
//
// @Summary  List pending passcode-reset requests
// @Tags     passcode
// @Produce  json
// @Success  200 {array} github_com_ddvk_rmfakecloud_internal_messages.PasscodeReset
// @Router   /passcode/resets [get]
// @Security BearerAuth
func (app *ReactAppWrapper) listPasscodeResets(c *gin.Context) {
	uid := userID(c)
	list := app.passcodeStore.ListForUser(uid)
	c.JSON(http.StatusOK, list)
}

// dismissPasscodeReset deletes a pending reset request without
// notifying the device.
//
// @Summary  Dismiss a pending passcode-reset request
// @Tags     passcode
// @Param    uuid path string true "Reset request UUID"
// @Success  200
// @Failure  400 {object} github_com_ddvk_rmfakecloud_internal_ui_viewmodel.ErrorResponse
// @Failure  404 {object} github_com_ddvk_rmfakecloud_internal_ui_viewmodel.ErrorResponse
// @Router   /passcode/resets/{uuid} [delete]
// @Security BearerAuth
func (app *ReactAppWrapper) dismissPasscodeReset(c *gin.Context) {
	uid := userID(c)
	requestID := c.Param("uuid")
	if requestID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if err := app.passcodeStore.Delete(uid, requestID); err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	log.Infof("%sdismissed reset request %s", passcodeLog, requestID)
	c.Status(http.StatusOK)
}

// approvePasscodeReset signs off on a pending reset request and pushes
// the approval to the originating device via the notification hub. Once
// approved, the device clears its passcode on next sync.
//
// @Summary  Approve a pending passcode-reset request
// @Tags     passcode
// @Param    uuid path string true "Reset request UUID"
// @Success  200
// @Failure  400 {object} github_com_ddvk_rmfakecloud_internal_ui_viewmodel.ErrorResponse
// @Failure  404 {object} github_com_ddvk_rmfakecloud_internal_ui_viewmodel.ErrorResponse
// @Router   /passcode/resets/{uuid}/approve [post]
// @Security BearerAuth
func (app *ReactAppWrapper) approvePasscodeReset(c *gin.Context) {
	uid := userID(c)
	requestID := c.Param("uuid")
	if requestID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	reset, err := app.passcodeStore.Approve(uid, requestID)
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	app.h.NotifyPasscodeReset(uid, reset.DeviceID, reset.DeviceName, reset.RequestID)
	log.Infof("%sapproved reset request %s for device %s", passcodeLog, reset.RequestID, reset.DeviceID)
	c.Status(http.StatusOK)
}
