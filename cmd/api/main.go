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
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"

	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
	"video-support-agent/internal/logger"
	"video-support-agent/internal/router"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const shutdownTimeout = 60 * time.Second

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()

	if err := config.ValidateElasticsearchConfig(cfg.Elasticsearch); err != nil {
		return err
	}

	trustedProxies, proxyErr := config.ValidateTrustedProxies(cfg.TrustedProxies)
	if proxyErr != nil {
		return proxyErr
	}
	cfg.TrustedProxies = trustedProxies

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

	searchClient, err := elasticsearch.NewClient(cfg.Elasticsearch)
	if err != nil {
		return errors.New("initialize Elasticsearch client failed")
	}
	defer searchClient.Close()

	searchRepository := repository.NewKnowledgeRepository(db)

	searchService, err := retrieval.NewService(
		searchClient,
		searchRepository,
	)
	if err != nil {
		return errors.New("initialize retrieval service failed")
	}

	appLogger.Info("retrieval dependencies initialized")

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

	modelTimeout, err := time.ParseDuration(cfg.LLM.Timeout)
	if err != nil || modelTimeout <= 0 || modelTimeout > 30*time.Second {
		return errors.New("LLM_TIMEOUT must be positive and no greater than 30s")
	}

	chatModel, err := llm.NewChatModel(context.Background(), cfg.LLM)
	if err != nil {
		return errors.New("initialize LLM model failed; check LLM configuration")
	}

	chatClient, err := llm.NewChatClient(chatModel, modelTimeout, appLogger)
	if err != nil {
		return errors.New("initialize chat client failed")
	}

	appLogger.Info("LLM client initialized")

	r := router.NewWithRetriever(cfg, db, tokenManager, contextStore, userLimiter, authLimiter, appLogger, chatClient, searchService)
	appLogger.Info("starting API server")
	appLogger.Info("server configuration", zap.String("app_name", cfg.AppName), zap.String("environment", cfg.Env), zap.String("port", cfg.Port), zap.String("log_level", cfg.LogLevel))

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      50 * time.Second,
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

	if err := shutdownHTTPServer(server, shutdownTimeout); err != nil {
		return err
	}

	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP server stopped unexpectedly")
	}

	appLogger.Info("API server stopped")
	return nil
}
