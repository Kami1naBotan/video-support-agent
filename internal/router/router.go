package router

import (
	"video-support-agent/internal/config"
	"video-support-agent/internal/handler"

	"github.com/gin-gonic/gin"
)

func New(cfg config.Config) *gin.Engine {
	r := gin.Default()

	healthHandler := handler.NewHealthHandler(cfg.AppName)

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": cfg.AppName + " is running",
		})
	})

	r.GET("/health", healthHandler.Check)

	return r
}
