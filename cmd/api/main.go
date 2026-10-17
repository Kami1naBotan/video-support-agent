package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/logger"
	"video-support-agent/internal/router"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	} else {
		gin.SetMode(gin.DebugMode)
	}

	appLogger, err := logger.New(cfg.LogLevel)
	if err != nil {
		return errors.New("initialize logger failed")
	}
	defer func() { _ = appLogger.Sync() }()

	jwtTTL, err := time.ParseDuration(cfg.JWT.TTL)
	if err != nil {
		return errors.New("initialize JWT_TTL failed")
	}

	tokenManager, err := auth.NewTokenManager(cfg.JWT.Secret, cfg.JWT.Issuer, jwtTTL)
	if err != nil {
		return errors.New("initialize JWT failed")
	}

	appLogger.Info("JWT configuration validated")

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		return errors.New("initialize database failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return errors.New("get database connection pool failed")
	}
	defer func() { _ = sqlDB.Close() }()
	appLogger.Info("database connected")

	redisClient := redis.NewClient(&redis.Options{
		Addr:                  cfg.Redis.Addr,
		Password:              cfg.Redis.Password,
		DB:                    cfg.Redis.DB,
		DialTimeout:           3 * time.Second,
		ReadTimeout:           time.Second,
		WriteTimeout:          time.Second,
		PoolTimeout:           time.Second,
		ContextTimeoutEnabled: true,
	})

	defer func() { _ = redisClient.Close() }()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), 3*time.Second)
	pingErr := redisClient.Ping(pingCtx).Err()
	cancelPing()

	if pingErr != nil {
		return errors.New("initialize redis failed; check Redis configuration and availability")
	}

	appLogger.Info("redis connected")

	contextStore, err := cache.NewContextStore(redisClient, 20, 30*time.Minute)
	if err != nil {
		return errors.New("initialize context store failed")
	}

	userLimiter, err := cache.NewUserRateLimiter(redisClient, 60, time.Minute)
	if err != nil {
		return errors.New("initialize user rate limiter failed")
	}

	authLimiter, err := cache.NewUserRateLimiter(redisClient, 10, time.Minute)
	if err != nil {
		return errors.New("initialize authentication rate limiter failed")
	}

	r := router.New(cfg, db, tokenManager, contextStore, userLimiter, authLimiter, appLogger)
	appLogger.Info("starting API server")
	appLogger.Info("server configuration", zap.String("app_name", cfg.AppName), zap.String("environment", cfg.Env), zap.String("port", cfg.Port), zap.String("log_level", cfg.LogLevel))

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}

	serverErrors := make(chan error, 1)

	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.New("HTTP server failed; check listen address and port")
		}
		return nil
	case <-signalCtx.Done():
		stop()
	}

	appLogger.Info("shutting down API server")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		return errors.New("HTTP graceful shutdown timed out or failed")
	}

	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP server stopped unexpectedly")
	}

	appLogger.Info("API server stopped")
	return nil
}
