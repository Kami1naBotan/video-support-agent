package router

import (
	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func New(cfg config.Config, db *gorm.DB) *gin.Engine {
	r := gin.Default()

	healthHandler := handler.NewHealthHandler(cfg.AppName)

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": cfg.AppName + " is running",
		})
	})

	r.GET("/health", healthHandler.Check)

	if db == nil {
		return r
	}

	conversationRepository := repository.NewConversationRepository(db)
	userRepository := repository.NewUserRepository(db)
	messageRepository := repository.NewMessageRepository(db)

	conversationService := service.NewConversationService(
		conversationRepository,
		userRepository,
	)
	messageService := service.NewMessageService(
		messageRepository,
		conversationRepository,
	)

	conversationHandler := handler.NewConversationHandler(conversationService)
	messageHandler := handler.NewMessageHandler(messageService)

	api := r.Group("/api/v1")
	conversations := api.Group("/conversations")
	conversations.POST("", conversationHandler.Create)
	conversations.GET("/:id", conversationHandler.GetByID)
	conversations.POST("/:id/messages", messageHandler.Create)
	conversations.GET("/:id/messages", messageHandler.List)

	return r
}
