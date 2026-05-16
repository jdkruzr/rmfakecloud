package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ddvk/rmfakecloud/internal/common"
	"github.com/ddvk/rmfakecloud/internal/integrations"
	"github.com/ddvk/rmfakecloud/internal/model"
	"github.com/ddvk/rmfakecloud/internal/storage"
	"github.com/ddvk/rmfakecloud/internal/storage/models"
	"github.com/ddvk/rmfakecloud/internal/ui/viewmodel"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	userIDContextKey    = "userID"
	browserIDContextKey = "browserID"
	isSync15Key         = "sync15"
	docIDParam          = "docid"
	intIDParam          = "intid"
	uiLogger            = "[ui] "
	ui10                = " [10] "
	useridParam         = "userid"
	cookieName          = ".Authrmfakecloud"
)

func userID(c *gin.Context) string {
	//TODO: suppress the warning
	//codeql[go/path-injection]
	return c.GetString(userIDContextKey)
}

// logout clears the auth cookie. The JWT itself is stateless and cannot be
// invalidated server-side; the client must discard it.
//
// @Summary  Log out (clear auth cookie)
// @Tags     auth
// @Success  200 {string} string "logged out"
// @Router   /logout [get]
func (app *ReactAppWrapper) logout(c *gin.Context) {
	c.SetCookie(cookieName, "/", -1, "", "", false, true)
	c.Status(http.StatusOK)
}

// triggerSync nudges the device-notification hub to wake any connected
// devices belonging to the caller. Used by the React UI after edits.
//
// @Summary  Trigger sync notification for connected devices
// @Tags     sync
// @Success  200 {string} string ""
// @Router   /sync [get]
// @Security BearerAuth
func (app *ReactAppWrapper) triggerSync(c *gin.Context) {
	uid := userID(c)
	br := c.GetString(browserIDContextKey)
	log.Info("browser", br)
	app.h.NotifySync(uid, br)
}

// register creates a new user account. Restricted to RegistrationOpen
// installations and to requests originating from localhost so admins can
// bootstrap the first user safely.
//
// @Summary  Register a new user account
// @Tags     auth
// @Accept   json
// @Produce  json
// @Param    credentials body viewmodel.LoginForm true "Email and password"
// @Success  200 {object} model.User
// @Failure  400 {object} viewmodel.ErrorResponse "registration closed or invalid body"
// @Failure  403 {object} viewmodel.ErrorResponse "registrations only accepted from localhost"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /register [post]
func (app *ReactAppWrapper) register(c *gin.Context) {

	if !app.cfg.RegistrationOpen {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	client := c.ClientIP()
	log.Info(client)

	if client != "localhost" &&
		client != "::1" &&
		client != "127.0.0.1" {
		c.AbortWithStatusJSON(http.StatusForbidden, viewmodel.NewErrorResponse("Registrations are closed"))
		return
	}

	var form viewmodel.LoginForm
	if err := c.ShouldBindJSON(&form); err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// Check this user doesn't already exist
	_, err := app.userStorer.GetUser(form.Email)
	if err == nil {
		badReq(c, "already taken")
		return
	}

	user, err := model.NewUser(form.Email, form.Password)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	err = app.userStorer.RegisterUser(user)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, user)
}

// login authenticates a user with email + password and returns a JWT.
// The same token is also set as the .Authrmfakecloud cookie. If the server
// was started with CreateFirstUser, the first successful login bootstraps
// an admin account for the supplied credentials.
//
// @Summary  Authenticate and obtain a JWT
// @Tags     auth
// @Accept   json
// @Produce  plain
// @Param    credentials body viewmodel.LoginForm true "Email and password"
// @Success  200 {string} string "JWT bearer token"
// @Failure  400 {string} string "invalid request body"
// @Failure  401 {string} string "wrong credentials"
// @Failure  500 {string} string "internal error"
// @Router   /login [post]
func (app *ReactAppWrapper) login(c *gin.Context) {
	var form viewmodel.LoginForm
	if err := c.ShouldBindJSON(&form); err != nil {
		log.Error(uiLogger, err)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	// not really thread safe
	if app.cfg.CreateFirstUser {
		log.Info("Creating an admin user")
		user, err := model.NewUser(form.Email, form.Password)
		if err != nil {
			log.Error("[login]", err)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		user.IsAdmin = true
		err = app.userStorer.RegisterUser(user)
		if err != nil {
			log.Error("[login] Register ", err)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		app.cfg.CreateFirstUser = false
	}

	// Try to find the user
	user, err := app.userStorer.GetUser(form.Email)
	if err != nil {
		log.Error(uiLogger, err, " cannot load user, login failed ip: ", c.ClientIP())
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if ok, err := user.CheckPassword(form.Password); err != nil || !ok {
		if err != nil {
			log.Error(err)
		} else if !ok {
			log.Warn(uiLogger, "wrong password for: ", form.Email, ", login failed ip: ", c.ClientIP())
		}
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	scopes := ""
	if user.Sync15 {
		scopes = isSync15Key
	}
	expiresAfter := 24 * time.Hour
	expires := time.Now().Add(expiresAfter)
	claims := &WebUserClaims{
		UserID:    user.ID,
		BrowserID: uuid.NewString(),
		Email:     user.Email,
		Scopes:    scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expires),
			Issuer:    "rmFake WEB",
			Audience:  []string{WebUsage},
		},
	}
	if user.IsAdmin {
		claims.Roles = []string{AdminRole}
	} else {
		claims.Roles = []string{"User"}
	}

	tokenString, err := common.SignClaims(claims, app.cfg.JWTSecretKey)

	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	log.Debug("cookie expires after: ", expiresAfter)
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(cookieName, tokenString, int(expiresAfter.Seconds()), "/", "", app.cfg.HTTPSCookie, true)

	c.String(http.StatusOK, tokenString)
}

// changePassword updates the caller's password. UserID in the body must
// match the authenticated user — admins cannot change another user's
// password through this endpoint (use PUT /users instead).
//
// @Summary  Change the authenticated user's password
// @Tags     profile
// @Accept   json
// @Produce  json
// @Param    request body     viewmodel.ResetPasswordForm true "Current + new password"
// @Success  200 {object} model.User
// @Failure  400 {object} viewmodel.ErrorResponse "wrong user, missing fields, or wrong current password"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /profile [post]
// @Security BearerAuth
func (app *ReactAppWrapper) changePassword(c *gin.Context) {
	var req viewmodel.ResetPasswordForm

	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}

	user, err := app.userStorer.GetUser(req.UserID)

	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	uid := userID(c)

	if user.ID != uid {
		log.Error("Trying to change password for a different user.")
		c.AbortWithStatusJSON(http.StatusBadRequest, viewmodel.NewErrorResponse("cant do that"))
		return
	}

	ok, err := user.CheckPassword(req.CurrentPassword)
	if !ok {
		if err != nil {
			log.Error(err)
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, viewmodel.NewErrorResponse("Invalid email or password"))
		return
	}

	if req.NewPassword != "" {
		user.SetPassword(req.NewPassword)
	}

	err = app.userStorer.UpdateUser(user)

	if err != nil {
		log.Error("error updating user", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, user)
}

// newCode generates a short one-time pairing code that a reMarkable device
// uses to register against this server. The code is single-use and short
// lived; check internal/app/codeconnector.go for the TTL.
//
// @Summary  Generate a device pairing code
// @Tags     auth
// @Produce  json
// @Success  200 {string} string "8-character pairing code"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /newcode [get]
// @Security BearerAuth
func (app *ReactAppWrapper) newCode(c *gin.Context) {
	uid := userID(c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error("Unable to find user: ", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, viewmodel.NewErrorResponse(err.Error()))
		return
	}

	code, err := app.codeConnector.NewCode(user.ID)
	if err != nil {
		log.Error("Unable to generate new device code: ", err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, viewmodel.NewErrorResponse("Unable to generate new code"))
		return
	}

	c.JSON(http.StatusOK, code)
}

func (app *ReactAppWrapper) getBackend(c *gin.Context) backend {
	s, ok := c.Get(backendVersionKey)
	if !ok {
		panic("key not set")
	}
	backend, ok := app.backends[s.(common.SyncVersion)]
	if !ok {
		panic("backend not found")
	}
	return backend
}

// listDocuments returns the authenticated user's document and folder tree,
// including the trash. Routed through the sync10 or sync15 backend based
// on the user's profile.
//
// @Summary  List documents
// @Tags     documents
// @Produce  json
// @Success  200 {object} viewmodel.DocumentTree
// @Failure  401 {object} viewmodel.ErrorResponse "missing or invalid auth"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /documents [get]
// @Security BearerAuth
func (app *ReactAppWrapper) listDocuments(c *gin.Context) {
	uid := userID(c)

	var tree *viewmodel.DocumentTree

	backend := app.getBackend(c)
	tree, err := backend.GetDocumentTree(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, tree)
}
// getDocument exports a document. The default `type=pdf` renders the
// notebook/PDF to a PDF stream; `type=rmdoc` returns a downloadable .rmdoc
// archive. The response body is the raw file bytes.
//
// @Summary  Export a document
// @Tags     documents
// @Produce  application/octet-stream
// @Param    docid path     string true  "Document UUID"
// @Param    type  query    string false "Export format" Enums(pdf, rmdoc) default(pdf)
// @Success  200 {file}     binary
// @Failure  500 {object}   viewmodel.ErrorResponse
// @Router   /documents/{docid} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getDocument(c *gin.Context) {
	uid := userID(c)
	docid := common.ParamS(docIDParam, c)

	exportType := c.DefaultQuery("type", "pdf")
	var exportOption storage.ExportOption = 0

	log.Info("exporting ", docid, " as ", exportType)
	backend := app.getBackend(c)

	reader, err := backend.Export(uid, docid, exportType, exportOption)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	defer reader.Close()

	if exportType == "rmdoc" {
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.rmdoc\"", docid))
	}

	c.DataFromReader(http.StatusOK, -1, "application/octet-stream", reader, nil)
}

// getDocumentMetadata is a placeholder — currently returns the literal
// string "TODO". Documented for surface completeness; clients should not
// depend on the response shape until this is implemented.
//
// @Summary  Get document metadata (placeholder)
// @Tags     documents
// @Produce  json
// @Param    docid path string true "Document UUID"
// @Success  200 {string} string "TODO"
// @Router   /documents/{docid}/metadata [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getDocumentMetadata(c *gin.Context) {
	uid := userID(c)
	docid := common.ParamS(docIDParam, c)
	// if err != nil {
	// 	log.Error(err)
	// 	c.AbortWithStatus(http.StatusInternalServerError)
	// 	return
	// }
	log.Info(uid, docid)
	c.JSON(http.StatusOK, "TODO")

}

// updateDocument moves or renames a document. Set ParentID to the target
// folder UUID (empty string or "root" puts it at the root, "trash" moves
// it to the trash). Name is the new display name.
//
// @Summary  Move or rename a document
// @Tags     documents
// @Accept   json
// @Produce  json
// @Param    update body     viewmodel.UpdateDoc true "Move/rename instructions"
// @Success  200
// @Failure  400 {object}    viewmodel.ErrorResponse
// @Router   /documents [put]
// @Security BearerAuth
func (app *ReactAppWrapper) updateDocument(c *gin.Context) {
	upd := viewmodel.UpdateDoc{}
	if err := c.ShouldBindJSON(&upd); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}
	backend := app.getBackend(c)
	uid := userID(c)
	log.Info(uiLogger, ui10, "updatedoc")
	err := backend.UpdateDocument(uid, upd.DocumentID, upd.Name, upd.ParentID)
	if err != nil {
		badReq(c, err.Error())
		return
	}

	c.Status(http.StatusOK)
}
// deleteDocument permanently deletes a document. There is no soft-delete
// from this endpoint; use PUT /documents with parent=trash to move-to-trash.
//
// @Summary  Delete a document
// @Tags     documents
// @Param    docid path string true "Document UUID"
// @Success  200
// @Failure  400 {object} viewmodel.ErrorResponse
// @Router   /documents/{docid} [delete]
// @Security BearerAuth
func (app *ReactAppWrapper) deleteDocument(c *gin.Context) {
	uid := userID(c)
	docid := c.Param("docid")
	backend := app.getBackend(c)

	err := backend.DeleteDocument(uid, docid)
	if err != nil {
		badReq(c, err.Error())
	}
	c.Status(http.StatusOK)
}

// createFolder creates a new folder under the supplied ParentID (empty
// string places it at the root).
//
// @Summary  Create a folder
// @Tags     documents
// @Accept   json
// @Produce  json
// @Param    folder body     viewmodel.NewFolder true "Folder name and parent"
// @Success  200 {object}    storage.Document
// @Failure  400 {object}    viewmodel.ErrorResponse
// @Failure  500 {object}    viewmodel.ErrorResponse
// @Router   /folders [post]
// @Security BearerAuth
func (app *ReactAppWrapper) createFolder(c *gin.Context) {
	upd := viewmodel.NewFolder{}
	if err := c.ShouldBindJSON(&upd); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}
	uid := userID(c)

	backend := app.getBackend(c)

	doc, err := backend.CreateFolder(uid, upd.Name, upd.ParentID)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, doc)
}

// createDocument uploads one or more files. Each file in the `file` form
// field becomes a document. Supply an optional `parent` form field with
// the target folder UUID (or "root").
//
// @Summary  Upload one or more documents
// @Tags     documents
// @Accept   multipart/form-data
// @Produce  json
// @Param    file   formData file   true  "Document file(s) to upload — repeat the field for multiple"
// @Param    parent formData string false "Parent folder UUID (omit or 'root' for root)"
// @Success  200 {array}  storage.Document
// @Failure  400 {object} viewmodel.ErrorResponse
// @Failure  409 {object} viewmodel.ErrorResponse "document with same name already exists (response includes docId)"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /documents/upload [post]
// @Security BearerAuth
func (app *ReactAppWrapper) createDocument(c *gin.Context) {
	uid := userID(c)
	log.Info("uploading documents from: ", uid)

	backend := app.getBackend(c)

	form, err := c.MultipartForm()
	if err != nil {
		log.Error(err)
		badReq(c, "not multiform")
		return
	}
	parentID := ""
	if parent, ok := form.Value["parent"]; ok {
		if parent[0] != "root" {
			parentID = parent[0]
		}
	}

	log.Info("Parent: " + parentID)

	docs := []*storage.Document{}
	for _, file := range form.File["file"] {
		f, err := file.Open()
		if err != nil {
			log.Error("[ui] ", err)
			badReq(c, "cant open attachment")
			return
		}

		defer f.Close()
		//do the stuff
		log.Info(uiLogger, fmt.Sprintf("Uploading %s , size: %d", file.Filename, file.Size))

		doc, err := backend.CreateDocument(uid, file.Filename, parentID, f)
		if err != nil {
			var existsErr *models.ErrDocumentExists
			if errors.As(err, &existsErr) {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": err.Error(), "docId": existsErr.DocID})
				return
			}
			log.Error(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		docs = append(docs, doc)
	}
	backend.Sync(uid)
	c.JSON(http.StatusOK, docs)
}

// getAppUsers returns every user on the instance. Admin-only.
//
// @Summary  List all users (admin)
// @Tags     admin
// @Produce  json
// @Success  200 {array}  viewmodel.User
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /users [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getAppUsers(c *gin.Context) {
	// Try to find the user
	users, err := app.userStorer.GetUsers()

	if err != nil {
		log.Error(err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, viewmodel.NewErrorResponse("Unable to get users."))
		return
	}

	uilist := make([]viewmodel.User, 0)
	for _, u := range users {
		usr := viewmodel.User{
			ID:        u.ID,
			Email:     u.Email,
			Name:      u.Name,
			CreatedAt: u.CreatedAt,
			IsAdmin:   u.IsAdmin,
		}
		uilist = append(uilist, usr)
	}
	c.JSON(http.StatusOK, uilist)
}

// getUser returns a single user by ID. Non-admins may only fetch their
// own profile; querying another user as a non-admin returns 401.
//
// @Summary  Get a user by ID (admin or self)
// @Tags     admin
// @Produce  json
// @Param    userid path string true "User ID"
// @Success  200 {object} viewmodel.User
// @Failure  400 {object} viewmodel.ErrorResponse
// @Failure  401 {string} string "querying a different user as non-admin"
// @Failure  404 {string} string "user not found"
// @Router   /users/{userid} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getUser(c *gin.Context) {
	uid := c.Param(useridParam)
	log.Info("Requested: ", uid)

	// Try to find the user
	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}

	if user == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, "Invalid user")
		return
	}
	if uid != user.ID && !IsAdmin(c) {
		log.Warn("Only admins can query other users")
		c.AbortWithStatusJSON(http.StatusUnauthorized, "")
		return
	}

	vmUser := &viewmodel.User{
		ID:        user.ID,
		Email:     user.Email,
		Name:      user.Name,
		CreatedAt: user.CreatedAt,
	}
	for _, i := range user.Integrations {
		vmUser.Integrations = append(vmUser.Integrations, i.Name)
	}

	c.JSON(http.StatusOK, vmUser)
}

// updateUser replaces a user's email and/or password. Admin-only.
//
// @Summary  Update a user (admin)
// @Tags     admin
// @Accept   json
// @Param    user body viewmodel.User true "User to update — ID must reference an existing user"
// @Success  202
// @Failure  400
// @Failure  404 {string} string "user not found"
// @Failure  500
// @Router   /users [put]
// @Security BearerAuth
func (app *ReactAppWrapper) updateUser(c *gin.Context) {
	var req viewmodel.User
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	user, err := app.userStorer.GetUser(req.ID)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	if user == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, "Invalid user")
		return
	}
	if req.NewPassword != "" {
		user.SetPassword(req.NewPassword)
	}

	if req.Email != "" {
		user.Email = req.Email
	}

	err = app.userStorer.UpdateUser(user)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusAccepted)
}
// deleteUser removes a user. The caller cannot delete themselves —
// trying to do so returns 500. Admin-only.
//
// @Summary  Delete a user (admin)
// @Tags     admin
// @Param    userid path string true "User ID"
// @Success  202
// @Failure  500 "cannot delete current user, or storage error"
// @Router   /users/{userid} [delete]
// @Security BearerAuth
func (app *ReactAppWrapper) deleteUser(c *gin.Context) {
	uid := c.Param(useridParam)
	if uid == userID(c) {
		log.Error("can't remove current user ")
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	err := app.userStorer.RemoveUser(uid)
	if err != nil {
		log.Error("can't remove ", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusAccepted)
}

// createUser provisions a new user account. Admin-only — the public
// /register endpoint is restricted to localhost.
//
// @Summary  Create a user (admin)
// @Tags     admin
// @Accept   json
// @Param    user body viewmodel.NewUser true "New user fields"
// @Success  201
// @Failure  400 {object} viewmodel.ErrorResponse
// @Failure  500
// @Router   /users [post]
// @Security BearerAuth
func (app *ReactAppWrapper) createUser(c *gin.Context) {
	var req viewmodel.NewUser
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}

	user, err := model.NewUser(req.ID, req.NewPassword)

	if err != nil {
		log.Error("can't create ", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	user.Email = req.Email

	err = app.userStorer.UpdateUser(user)
	if err != nil {
		log.Error("can't create ", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusCreated)
}

// listIntegrations returns the caller's configured cloud-storage integrations
// (WebDAV, FTP, Dropbox, etc.). Secrets in the response are NOT scrubbed —
// callers should treat the payload as sensitive.
//
// @Summary  List the user's integrations
// @Tags     integrations
// @Produce  json
// @Success  200 {array}  model.IntegrationConfig
// @Failure  401 {object} viewmodel.ErrorResponse
// @Router   /integrations [get]
// @Security BearerAuth
func (app *ReactAppWrapper) listIntegrations(c *gin.Context) {
	uid := userID(c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	c.JSON(http.StatusOK, user.Integrations)
}

func warnLocalfsEdition(c *gin.Context, int *model.IntegrationConfig) {
	s, err := yaml.Marshal(gin.H{"integrations": []*model.IntegrationConfig{int}})
	if err != nil {
		log.Error("error updating user", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.AbortWithStatusJSON(http.StatusForbidden,
		viewmodel.NewErrorResponse("To avoid security issues with local directory integration, you have to manually edit your .userprofile file:\n\n"+string(s)))
}

// createIntegration adds a new storage integration to the caller's profile.
// The Localfs provider is rejected with 403 — its config must be edited
// manually in the user profile file to prevent path-traversal abuse.
//
// @Summary  Create a storage integration
// @Tags     integrations
// @Accept   json
// @Produce  json
// @Param    integration body     model.IntegrationConfig true "Integration configuration"
// @Success  200 {object} model.IntegrationConfig
// @Failure  400 {object} viewmodel.ErrorResponse
// @Failure  401 {object} viewmodel.ErrorResponse
// @Failure  403 {object} viewmodel.ErrorResponse "Localfs provider — edit user profile manually"
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations [post]
// @Security BearerAuth
func (app *ReactAppWrapper) createIntegration(c *gin.Context) {
	int := model.IntegrationConfig{}
	if err := c.ShouldBindJSON(&int); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}

	if int.Provider == integrations.LocalfsProvider {
		int.ID = uuid.NewString()
		warnLocalfsEdition(c, &int)
		return
	}

	uid := userID(c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	int.ID = uuid.NewString()
	user.Integrations = append(user.Integrations, int)

	err = app.userStorer.UpdateUser(user)

	if err != nil {
		log.Error("error updating user", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, int)
}

// getIntegration returns a single integration by ID.
//
// @Summary  Get an integration
// @Tags     integrations
// @Produce  json
// @Param    intid path string true "Integration UUID"
// @Success  200 {object} model.IntegrationConfig
// @Failure  401 {object} viewmodel.ErrorResponse
// @Failure  404
// @Router   /integrations/{intid} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getIntegration(c *gin.Context) {
	uid := userID(c)

	intid := common.ParamS(intIDParam, c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	for _, integration := range user.Integrations {
		if integration.ID == intid {
			c.JSON(http.StatusOK, integration)
			return
		}
	}

	c.AbortWithStatus(http.StatusNotFound)
}

// updateIntegration replaces an existing integration's config. As with
// create, Localfs cannot be edited through the API.
//
// @Summary  Update an integration
// @Tags     integrations
// @Accept   json
// @Produce  json
// @Param    intid       path     string                  true "Integration UUID"
// @Param    integration body     model.IntegrationConfig true "Updated configuration"
// @Success  200 {object} model.IntegrationConfig
// @Failure  400 {object} viewmodel.ErrorResponse
// @Failure  401 {object} viewmodel.ErrorResponse
// @Failure  403 {object} viewmodel.ErrorResponse "Localfs provider — edit user profile manually"
// @Failure  404
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations/{intid} [put]
// @Security BearerAuth
func (app *ReactAppWrapper) updateIntegration(c *gin.Context) {
	int := model.IntegrationConfig{}
	if err := c.ShouldBindJSON(&int); err != nil {
		log.Error(err)
		badReq(c, err.Error())
		return
	}

	if int.Provider == integrations.LocalfsProvider {
		warnLocalfsEdition(c, &int)
		return
	}

	uid := userID(c)

	intid := common.ParamS(intIDParam, c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	for idx, integration := range user.Integrations {
		if integration.ID == intid {
			int.ID = integration.ID
			user.Integrations[idx] = int

			err = app.userStorer.UpdateUser(user)

			if err != nil {
				log.Error("error updating user", err)
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}

			c.JSON(http.StatusOK, int)
			return
		}
	}

	c.AbortWithStatus(http.StatusNotFound)
}

// deleteIntegration removes the integration from the caller's profile.
//
// @Summary  Delete an integration
// @Tags     integrations
// @Param    intid path string true "Integration UUID"
// @Success  202
// @Failure  401 {object} viewmodel.ErrorResponse
// @Failure  404
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations/{intid} [delete]
// @Security BearerAuth
func (app *ReactAppWrapper) deleteIntegration(c *gin.Context) {
	uid := userID(c)

	intid := common.ParamS(intIDParam, c)

	user, err := app.userStorer.GetUser(uid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	for idx, integration := range user.Integrations {
		if integration.ID == intid {
			user.Integrations = append(user.Integrations[:idx], user.Integrations[idx+1:]...)

			err = app.userStorer.UpdateUser(user)

			if err != nil {
				log.Error("error updating user", err)
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}

			c.Status(http.StatusAccepted)
			return
		}
	}

	c.AbortWithStatus(http.StatusNotFound)
}

// exploreIntegration lists the contents of a folder on the configured
// remote (WebDAV, FTP, Dropbox, ...). Path is the integration-relative
// path; missing or empty paths default to "root".
//
// @Summary  Browse a folder on a configured integration
// @Tags     integrations
// @Produce  json
// @Param    intid path string true  "Integration UUID"
// @Param    path  path string false "Folder path (use leading slash; empty for root)"
// @Success  200 {object} github_com_ddvk_rmfakecloud_internal_messages.IntegrationFolder
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations/{intid}/explore/{path} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) exploreIntegration(c *gin.Context) {
	uid := userID(c)

	integrationID := common.ParamS(intIDParam, c)

	integrationProvider, err := integrations.GetStorageIntegrationProvider(app.userStorer, uid, integrationID)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	folder := common.ParamS("path", c)
	if folder == "" {
		folder = "root"
	}

	response, err := integrationProvider.List(folder, 2)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, response)
}

// getMetadataIntegration returns metadata for a single remote file
// without downloading it.
//
// @Summary  Get metadata for a file on a configured integration
// @Tags     integrations
// @Produce  json
// @Param    intid path string true "Integration UUID"
// @Param    path  path string true "Remote file path"
// @Success  200 {object} github_com_ddvk_rmfakecloud_internal_messages.IntegrationMetadata
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations/{intid}/metadata/{path} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) getMetadataIntegration(c *gin.Context) {
	uid := userID(c)

	integrationID := common.ParamS(intIDParam, c)

	integrationProvider, err := integrations.GetStorageIntegrationProvider(app.userStorer, uid, integrationID)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	fileid := common.ParamS("path", c)

	response, err := integrationProvider.GetMetadata(fileid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	c.JSON(http.StatusOK, response)
}

// downloadThroughIntegration streams a remote file's raw bytes back to
// the caller. Content-Type is not set — assume application/octet-stream.
//
// @Summary  Download a file from a configured integration
// @Tags     integrations
// @Produce  application/octet-stream
// @Param    intid path string true "Integration UUID"
// @Param    path  path string true "Remote file path"
// @Success  200 {file}   binary
// @Failure  500 {object} viewmodel.ErrorResponse
// @Router   /integrations/{intid}/download/{path} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) downloadThroughIntegration(c *gin.Context) {
	uid := userID(c)

	integrationID := common.ParamS(intIDParam, c)

	integrationProvider, err := integrations.GetStorageIntegrationProvider(app.userStorer, uid, integrationID)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	fileid := common.ParamS("path", c)

	response, size, err := integrationProvider.Download(fileid)
	if err != nil {
		log.Error(err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	defer response.Close()

	c.DataFromReader(http.StatusOK, size, "", response, nil)
}

// screenshareJoinActive returns the caller's active screenshare room (if
// any) along with the ICE servers a WebRTC viewer should use.
//
// @Summary  Get the caller's active screenshare room
// @Tags     screenshare
// @Produce  json
// @Success  200 {object} viewmodel.ScreenshareRoom
// @Failure  404 {object} viewmodel.ErrorResponse "no active room"
// @Router   /screenshare/room [get]
// @Security BearerAuth
func (app *ReactAppWrapper) screenshareJoinActive(c *gin.Context) {
	uid := userID(c)

	roomID := app.roomManager.FindActiveRoom(uid)
	if roomID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active room"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"roomId":     roomID,
		"clients":    app.roomManager.GetClients(roomID),
		"iceServers": app.cfg.ICEServers,
	})
}

// screenshareGetRoom returns the state of a specific room by ID.
//
// @Summary  Get a screenshare room by ID
// @Tags     screenshare
// @Produce  json
// @Param    roomId path string true "Room ID"
// @Success  200 {object} viewmodel.ScreenshareRoom
// @Failure  404 {object} viewmodel.ErrorResponse "room not found"
// @Router   /screenshare/room/{roomId} [get]
// @Security BearerAuth
func (app *ReactAppWrapper) screenshareGetRoom(c *gin.Context) {
	roomID := c.Param("roomId")

	room := app.roomManager.GetRoom(roomID)
	if room == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"roomId":    room.RoomID,
		"createdAt": room.CreatedAt.Format(time.RFC3339Nano),
		"clients":   app.roomManager.GetClients(roomID),
	})
}

// screenshareGetOffer broadcasts an offer-request to the device that owns
// the active screenshare room and waits up to 30s for a signaling message
// back. Returns the offer + ICE servers when the device responds.
//
// @Summary  Request a WebRTC offer from the active screenshare session
// @Tags     screenshare
// @Produce  json
// @Success  200 {object} viewmodel.ScreenshareOffer
// @Failure  404 {object} viewmodel.ErrorResponse "no active room"
// @Failure  504 {object} viewmodel.ErrorResponse "device did not respond within 30s"
// @Router   /screenshare/offer [get]
// @Security BearerAuth
func (app *ReactAppWrapper) screenshareGetOffer(c *gin.Context) {
	uid := userID(c)
	clientID := c.GetString(browserIDContextKey)

	roomID := app.roomManager.FindActiveRoom(uid)
	if roomID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active room"})
		return
	}

	app.roomManager.AddBroadcast(roomID, clientID, json.RawMessage(`{"type":"request-offer","clientId":"`+clientID+`"}`))

	var inner map[string]interface{}
	json.Unmarshal([]byte(`{"type":"request-offer","clientId":"`+clientID+`","sourceDeviceID":"`+clientID+`"}`), &inner)
	app.h.NotifyScreenshare(uid, clientID, inner)

	if app.mqtt != nil && app.mqtt.HasConnectedClient(uid) {
		clients := app.roomManager.GetClients(roomID)
		for _, cl := range clients {
			if cl.IsOwner {
				mqttMsg, _ := json.Marshal(map[string]interface{}{
					"type":     "broadcast",
					"clientId": clientID,
					"payload":  json.RawMessage(`{"type":"request-offer","clientId":"` + clientID + `"}`),
				})
				app.mqtt.PublishSignaling(uid, cl.ClientID, mqttMsg)
				break
			}
		}
	}

	msgs := app.roomManager.WaitForMessages(roomID, 1, 30*time.Second)
	if msgs == nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout waiting for offer"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"roomId":     roomID,
		"messages":   msgs,
		"iceServers": app.cfg.ICEServers,
	})
}

// screenshareSendAnswer relays a viewer's WebRTC answer (or ICE
// candidate) back to the broadcasting device.
//
// @Summary  Send a WebRTC answer to a screenshare room
// @Tags     screenshare
// @Accept   json
// @Param    roomId path     string                       true "Room ID"
// @Param    answer body     viewmodel.ScreenshareAnswer  true "Signaling payload + target client ID"
// @Success  202
// @Failure  400 {object} viewmodel.ErrorResponse "invalid body"
// @Failure  404 {object} viewmodel.ErrorResponse "room not found"
// @Router   /screenshare/room/{roomId}/answer [post]
// @Security BearerAuth
func (app *ReactAppWrapper) screenshareSendAnswer(c *gin.Context) {
	roomID := c.Param("roomId")
	clientID := c.GetString(browserIDContextKey)
	uid := userID(c)

	if !app.roomManager.RoomExists(roomID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "room not found"})
		return
	}

	var msg struct {
		Payload        json.RawMessage `json:"payload"`
		TargetClientID string          `json:"targetClientId"`
	}
	if err := c.ShouldBindJSON(&msg); err != nil {
		badReq(c, "invalid body")
		return
	}

	app.roomManager.AddDirect(roomID, clientID, msg.TargetClientID, msg.Payload)

	var inner map[string]interface{}
	json.Unmarshal(msg.Payload, &inner)
	inner["sourceDeviceID"] = clientID
	app.h.NotifyScreenshare(uid, clientID, inner)

	if app.mqtt != nil && app.mqtt.HasConnectedClient(uid) {
		mqttMsg, _ := json.Marshal(map[string]interface{}{
			"type":     "direct",
			"clientId": clientID,
			"payload":  json.RawMessage(msg.Payload),
		})
		app.mqtt.PublishSignaling(uid, msg.TargetClientID, mqttMsg)
	}

	c.Status(http.StatusAccepted)
}

// screenshareDeleteRoom tears down every screenshare room owned by the
// caller. The roomId path param is accepted but ignored — deletion is
// scoped to the user.
//
// @Summary  Tear down all screenshare rooms for the caller
// @Tags     screenshare
// @Param    roomId path string true "Room ID (currently ignored — all caller rooms are deleted)"
// @Success  204
// @Router   /screenshare/room/{roomId} [delete]
// @Security BearerAuth
func (app *ReactAppWrapper) screenshareDeleteRoom(c *gin.Context) {
	uid := userID(c)
	app.roomManager.DeleteAllForUser(uid)
	c.Status(http.StatusNoContent)
}

