// @title                      rmfakecloud Admin API
// @version                    1.0
// @description                JSON API consumed by the React admin UI and available to external integrators. The device-facing sync API (under /sync, /document-storage, etc.) is intentionally NOT documented here — those paths are a firmware contract.
// @BasePath                   /ui/api
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description.BearerAuth     JWT obtained from POST /login, sent as `Authorization: Bearer <token>` or via the .Authrmfakecloud cookie.
//
//go:generate swag init -g routes.go -o docs --parseDependency --parseInternal
package ui

import (
	"net/http"
	"strings"

	// The generated docs package's init() registers the OpenAPI spec with
	// swag's global registry, so gin-swagger and the JSON alias below can
	// both find it.
	"github.com/ddvk/rmfakecloud/internal/ui/docs"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

// RegisterRoutes the apps routes
func (app *ReactAppWrapper) RegisterRoutes(router *gin.Engine) {
	router.StaticFS(app.prefix, app)

	router.GET("/favicon.ico", func(c *gin.Context) {
		c.FileFromFS("/favicon.ico", app.fs)
	})
	router.GET("/robots.txt", func(c *gin.Context) {
		c.FileFromFS("/robots.txt", app.fs)
	})

	//hack for index.html
	router.NoRoute(func(c *gin.Context) {
		uri := c.Request.RequestURI
		log.Info(uri)
		if strings.HasPrefix(uri, "/api") ||
			strings.HasPrefix(uri, "/ui/api") ||
			c.Request.Method != http.MethodGet {

			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		c.FileFromFS(indexReplacement, app)
	})

	r := router.Group("/ui/api")

	if app.cfg.EnableOpenAPI {
		log.Info("OpenAPI spec available at /ui/api/openapi.json and Swagger UI at /ui/api/docs/")
		// /openapi.json — stable, tool-friendly alias for client generators.
		r.GET("/openapi.json", func(c *gin.Context) {
			c.Data(http.StatusOK, "application/json", []byte(docs.SwaggerInfo.ReadDoc()))
		})
		// /docs and /docs/ → /docs/index.html (gin-swagger doesn't serve the
		// directory listing itself).
		r.GET("/docs", func(c *gin.Context) {
			c.Redirect(http.StatusFound, "/ui/api/docs/index.html")
		})
		// /docs/* — interactive Swagger UI (also exposes the spec at /docs/doc.json).
		r.GET("/docs/*any", func(c *gin.Context) {
			if c.Param("any") == "/" {
				c.Redirect(http.StatusFound, "/ui/api/docs/index.html")
				return
			}
			ginSwagger.WrapHandler(swaggerFiles.Handler)(c)
		})
	}

	r.POST("register", app.register)
	r.POST("login", app.login)
	r.GET("logout", app.logout)
	//with authentication
	auth := r.Group("")
	auth.Use(app.authMiddleware())
	auth.HEAD("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	auth.GET("sync", app.triggerSync)

	auth.GET("newcode", app.newCode)

	// passcode (PIN) reset approval
	auth.GET("passcode/resets", app.listPasscodeResets)
	auth.POST("passcode/resets/:uuid/approve", app.approvePasscodeReset)
	auth.DELETE("passcode/resets/:uuid", app.dismissPasscodeReset)

	// auth.GET("profile", app.newCode)
	auth.POST("profile", app.changePassword)
	// auth.POST("changeEmail", app.changePassword)

	auth.GET("documents", app.listDocuments)
	auth.GET("documents/:docid", app.getDocument)
	auth.POST("documents/upload", app.createDocument)

	//move, rename
	auth.DELETE("documents/:docid", app.deleteDocument)
	auth.PUT("documents", app.updateDocument)
	auth.POST("folders", app.createFolder)
	auth.GET("documents/:docid/metadata", app.getDocumentMetadata)

	// integrations
	auth.GET("integrations", app.listIntegrations)
	auth.POST("integrations", app.createIntegration)
	auth.GET("integrations/:intid", app.getIntegration)
	auth.PUT("integrations/:intid", app.updateIntegration)
	auth.DELETE("integrations/:intid", app.deleteIntegration)

	auth.GET("integrations/:intid/explore/*path", app.exploreIntegration)
	auth.GET("integrations/:intid/metadata/*path", app.getMetadataIntegration)
	auth.GET("integrations/:intid/download/*path", app.downloadThroughIntegration)

	ss := auth.Group("screenshare")
	ss.GET("room", app.screenshareJoinActive)
	ss.GET("room/:roomId", app.screenshareGetRoom)
	ss.GET("offer", app.screenshareGetOffer)
	ss.POST("room/:roomId/answer", app.screenshareSendAnswer)
	ss.DELETE("room/:roomId", app.screenshareDeleteRoom)

	//admin
	admin := auth.Group("")
	admin.Use(app.adminMiddleware())
	admin.GET("users/:userid", app.getUser)
	admin.DELETE("users/:userid", app.deleteUser)
	admin.PUT("users", app.updateUser)
	admin.POST("users", app.createUser)
	admin.GET("users", app.getAppUsers)
}
