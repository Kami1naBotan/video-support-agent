package main

import (
	"log"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/logger"
	"video-support-agent/internal/router"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func main() {
	cfg := config.Load()

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	} else {
		gin.SetMode(gin.DebugMode)
	}

	appLogger, err := logger.New(cfg.LogLevel)
	if err != nil {
		log.Fatalf("initialize logger: %v", err)
	}
	defer func() {
		_ = appLogger.Sync()
	}()

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		appLogger.Error("initialize database failed", zap.Error(err))
		return
	}

	sqlDB, err := db.DB()
	if err != nil {
		appLogger.Error("get database connection pool failed", zap.Error(err))
		return
	}
	defer func() {
		_ = sqlDB.Close()
	}()

	appLogger.Info("database connected")

	r := router.New(cfg, db)

	appLogger.Info("starting API server")

	appLogger.Info(
		"server configuration",
		zap.String("app_name", cfg.AppName),
		zap.String("environment", cfg.Env),
		zap.String("port", cfg.Port),
		zap.String("log_level", cfg.LogLevel),
	)

	if err := r.Run(":" + cfg.Port); err != nil {
		appLogger.Fatal("API server stopped", zap.Error(err))
	}
}
