package router

import (
	"time"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func New(cfg config.Config, db *gorm.DB, tokens *auth.TokenManager, contexts service.MessageContextStore, limiter middleware.UserLimiter, authLimiter middleware.AuthIPLimiter, appLogger *zap.Logger) *gin.Engine {
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		panic("invalid trusted proxy configuration")
	}
	r.Use(middleware.SafeAccessLog(appLogger), middleware.SafeRecovery(appLogger))

	healthHandler := handler.NewHealthHandler(cfg.AppName)
	r.GET("/", func(c *gin.Context) { c.JSON(200, gin.H{"message": cfg.AppName + " is running"}) })
	r.GET("/health", healthHandler.Check)

	if db == nil {
		return r
	}

	conversationRepository := repository.NewConversationRepository(db)
	userRepository := repository.NewUserRepository(db)

	currentUserService := service.NewCurrentUserService(userRepository)
	currentUserHandler := handler.NewCurrentUserHandler(currentUserService)

	authService := service.NewAuthService(userRepository)
	authHandler := handler.NewAuthHandler(authService, tokens)

	messageRepository := repository.NewMessageRepository(db)
	conversationService := service.NewConversationService(conversationRepository, userRepository)

	messageService := service.NewMessageService(messageRepository, conversationRepository, contexts, appLogger)
	conversationHandler := handler.NewConversationHandler(conversationService)

	messageHandler := handler.NewMessageHandler(messageService)

	api := r.Group("/api/v1")
	api.Use(middleware.BodyLimit(64*1024), middleware.RequestDeadline(5*time.Second))

	authRoutes := api.Group("/auth")
	authRoutes.Use(middleware.AuthRateLimit(authLimiter, appLogger))
	authRoutes.POST("/register", authHandler.Register)
	authRoutes.POST("/login", authHandler.Login)

	protected := api.Group("")
	protected.Use(middleware.JWTAuth(tokens), middleware.UserRateLimit(limiter, appLogger))
	protected.GET("/me", currentUserHandler.Me)

	staff := protected.Group("/staff")
	staff.Use(middleware.RequireRoles(currentUserService, model.RoleSupport, model.RoleAdmin))
	staff.GET("/me", currentUserHandler.Me)

	admin := protected.Group("/admin")
	admin.Use(middleware.RequireRoles(currentUserService, model.RoleAdmin))
	admin.GET("/me", currentUserHandler.Me)

	conversations := protected.Group("/conversations")
	conversations.POST("", conversationHandler.Create)
	conversations.GET("/:id", conversationHandler.GetByID)
	conversations.POST("/:id/messages", messageHandler.Create)
	conversations.GET("/:id/messages", messageHandler.List)
	return r
}
