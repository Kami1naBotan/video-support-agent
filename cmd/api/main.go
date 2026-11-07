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
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/database"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/generationquery"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/logger"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/router"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

const (
	shutdownTimeout        = 60 * time.Second
	vectorRetrievalTimeout = 35 * time.Second
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()

	appealSigner, err := confirmation.NewAppealSigner(cfg.AppealConfirmation)
	if err != nil {
		return errors.New("initialize appeal confirmation failed; check APPEAL_CONFIRMATION_KEY_BASE64, APPEAL_CONFIRMATION_ISSUER and APPEAL_CONFIRMATION_AUDIENCE")
	}

	if err := config.ValidateElasticsearchConfig(cfg.Elasticsearch); err != nil {
		return err
	}

	if err := config.ValidateEmbeddingConfig(cfg.Embedding); err != nil {
		return err
	}

	if err := config.ValidateMilvusConfig(cfg.Milvus); err != nil {
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

	obs, err := observability.New(context.Background(), cfg.Observability, "api")
	if err != nil {
		return err
	}
	defer func() {
		if err := obs.Shutdown(context.Background()); err != nil {
			appLogger.Warn("observability shutdown incomplete")
		}
	}()

	metricsServer, err := observability.StartMetrics(obs)
	if err != nil {
		return err
	}
	defer func() {
		if err := metricsServer.Shutdown(context.Background()); err != nil {
			appLogger.Warn("metrics shutdown incomplete")
		}
	}()

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

	indexReady, err := checkKnowledgeAPIStartup(context.Background(), db, cfg, os.Getenv("KNOWLEDGE_SOURCE_SERVER_UUID"))
	if err != nil {
		return err
	}

	appLogger.Info("knowledge startup checks passed", zap.Bool("active_generation_ready", indexReady))

	esObserver, err := observability.NewQueryObserver(obs, "elasticsearch")
	if err != nil {
		return err
	}

	searchClient, err := elasticsearch.NewClient(cfg.Elasticsearch, esObserver)
	if err != nil {
		return errors.New("initialize Elasticsearch client failed")
	}
	defer searchClient.Close()

	searchRepository := repository.NewKnowledgeRepository(db)

	activeRepository, err := repository.NewKnowledgeActiveRepository(db)
	if err != nil {
		return errors.New("initialize active generation repository failed")
	}

	embeddingClient, err := embedding.NewClient(cfg.Embedding)
	if err != nil {
		return errors.New("initialize embedding client failed")
	}
	defer embeddingClient.Close()

	milvusObserver, err := observability.NewQueryObserver(obs, "milvus")
	if err != nil {
		return err
	}

	vectorClient, err := milvus.NewClient(cfg.Milvus, milvusObserver)
	if err != nil {
		return errors.New("initialize Milvus client failed")
	}
	defer vectorClient.Close()

	vectorSearcher, err := milvus.NewSearcher(embeddingClient, vectorClient, vectorRetrievalTimeout)
	if err != nil {
		return errors.New("initialize vector candidate searcher failed")
	}

	retrievers, err := generationquery.NewRetrievers(activeRepository, searchClient, vectorSearcher, searchRepository, searchRepository, nil, vectorRetrievalTimeout)
	if err != nil {
		return errors.New("initialize generation retrieval failed")
	}

	appLogger.Info("generation retrieval dependencies initialized")

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

	llmObserver, err := observability.NewLLMObserver(obs)
	if err != nil {
		return err
	}

	agentModels, err := llm.NewAgentModels(context.Background(), cfg.LLM, llmObserver)
	if err != nil {
		return errors.New("initialize Agent models failed; check LLM_BASE_URL, LLM_MODEL, LLM_API_KEY and LLM_TIMEOUT (0 < timeout <= 30s)")
	}

	loggedHybrid, err := retrieval.NewLoggedHybridRetriever(retrievers.Hybrid, appLogger)
	if err != nil {
		return errors.New("initialize hybrid retrieval logging failed")
	}

	executionObserver, err := observability.NewExecutionObserver(obs)
	if err != nil {
		return err
	}

	messageHandler, err := newAgentMessageHandler(db, contextStore, loggedHybrid, agentModels, appealSigner, appLogger, executionObserver)
	if err != nil {
		return err
	}

	appLogger.Info("Agent chat dependencies initialized")

	httpObservation, err := middleware.NewHTTPObservation(obs)
	if err != nil {
		return err
	}

	r := router.NewWithMessageHandler(
		cfg,
		db,
		tokenManager,
		contextStore,
		userLimiter,
		authLimiter,
		appLogger,
		nil,
		retrievers.Keyword,
		retrievers.Vector,
		messageHandler,
		httpObservation,
	)
	appLogger.Info("starting API server")
	appLogger.Info("server configuration",
		zap.String("app_name", cfg.AppName),
		zap.String("environment", cfg.Env),
		zap.String("port", cfg.Port),
		zap.String("log_level", cfg.LogLevel))

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

	var stopReason error

	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			stopReason = errors.New("HTTP server failed; check listen address and port")
		}
		if err := shutdownHTTPServer(server, shutdownTimeout); err != nil {
			return errors.Join(stopReason, err)
		}
		return stopReason
	case err := <-metricsServer.Errors():
		stopReason = err
		stop()
	case <-signalCtx.Done():
		stop()
	}

	appLogger.Info("shutting down API server")

	if err := shutdownHTTPServer(server, shutdownTimeout); err != nil {
		return errors.Join(stopReason, err)
	}

	if err := <-serverErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP server stopped unexpectedly")
	}

	appLogger.Info("API server stopped")
	return stopReason
}
