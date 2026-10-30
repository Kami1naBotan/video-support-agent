package router

import (
	"time"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

func New(cfg config.Config, db *gorm.DB, tokens *auth.TokenManager, contexts service.MessageContextStore, limiter middleware.UserLimiter, authLimiter middleware.AuthIPLimiter, appLogger *zap.Logger, chat service.ChatResponder) *gin.Engine {
	return NewWithRetriever(cfg, db, tokens, contexts, limiter, authLimiter, appLogger, chat, nil)
}

func NewWithRetriever(cfg config.Config, db *gorm.DB, tokens *auth.TokenManager, contexts service.MessageContextStore, limiter middleware.UserLimiter, authLimiter middleware.AuthIPLimiter, appLogger *zap.Logger, chat service.ChatResponder, retriever retrieval.Retriever) *gin.Engine {
	r := gin.New()

	trustedProxies, err := config.ValidateTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		panic("invalid trusted proxy configuration")
	}
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
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

	messageService := service.NewMessageService(messageRepository, conversationRepository, contexts, appLogger, chat)
	conversationHandler := handler.NewConversationHandler(conversationService)

	messageHandler := handler.NewMessageHandler(messageService)

	knowledgeRepository := repository.NewKnowledgeRepository(db)
	knowledgeService := service.NewKnowledgeService(knowledgeRepository)
	knowledgeHandler := handler.NewKnowledgeHandler(knowledgeService)
	knowledgeSearchHandler := handler.NewKnowledgeSearchHandler(retriever)

	api := r.Group("/api/v1")
	api.Use(middleware.BodyLimit(64*1024), middleware.RequestDeadline(40*time.Second))

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

	knowledge := admin.Group("/knowledge")

	knowledge.POST("/documents", knowledgeHandler.Create)
	knowledge.POST("/documents/:document_id/versions", knowledgeHandler.CreateRevision)
	knowledge.GET("/documents/:document_id", knowledgeHandler.GetDocumentState)
	knowledge.POST("/documents/:document_id/publish", knowledgeHandler.Publish)
	knowledge.POST("/documents/:document_id/withdraw", knowledgeHandler.Withdraw)
	knowledge.GET("/versions/:version_id", knowledgeHandler.GetByID)
	knowledge.PUT("/versions/:version_id", knowledgeHandler.Edit)
	knowledge.POST("/versions/:version_id/verifications", knowledgeHandler.Verify)

	currentKnowledge := protected.Group("/knowledge")
	currentKnowledge.Use(middleware.RequireRoles(currentUserService, model.RoleUser, model.RoleSupport, model.RoleAdmin))
	currentKnowledge.GET("/search", knowledgeSearchHandler.Search)
	currentKnowledge.GET("/documents/:document_id/current", knowledgeHandler.GetCurrent)
	currentKnowledge.GET("/versions/:version_id", knowledgeHandler.GetHistory)
	currentKnowledge.GET("/versions/:version_id/articles/:article_id", knowledgeHandler.GetHistoryArticle)

	conversations := protected.Group("/conversations")
	conversations.POST("", conversationHandler.Create)
	conversations.GET("", conversationHandler.List)
	conversations.GET("/:id", conversationHandler.GetByID)
	conversations.POST("/:id/messages", messageHandler.Create)
	conversations.POST("/:id/messages/stream", messageHandler.CreateStream)
	conversations.GET("/:id/messages", messageHandler.List)

	return r
}
