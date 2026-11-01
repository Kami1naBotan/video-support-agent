package config

import (
	"os"
	"strings"
)

type Config struct {
	AppName        string
	Env            string
	Port           string
	LogLevel       string
	Database       DatabaseConfig
	Redis          RedisConfig
	JWT            JWTConfig
	LLM            LLMConfig
	Elasticsearch  ElasticsearchConfig
	Embedding      EmbeddingConfig
	Milvus         MilvusConfig
	TrustedProxies []string
}

type DatabaseConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type JWTConfig struct {
	Secret string
	Issuer string
	TTL    string
}

type LLMConfig struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout string
}

type ElasticsearchConfig struct {
	URL        string
	IndexAlias string
	Timeout    string
}

type EmbeddingConfig struct {
	URL           string
	Model         string
	ModelDigest   string
	Dimensions    string
	ContextLength string
	Timeout       string
}

type MilvusConfig struct {
	URL             string
	Database        string
	CollectionAlias string
	Timeout         string
}

func Load() Config {
	return Config{
		AppName:  getEnv("APP_NAME", "bilibili-support-agent-api"),
		Env:      getEnv("APP_ENV", "development"),
		Port:     getEnv("APP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "127.0.0.1"),
			Port:     getEnv("DB_PORT", "3307"),
			User:     getEnv("DB_USER", "bili_app"),
			Password: os.Getenv("DB_PASSWORD"),
			Name:     getEnv("DB_NAME", "bili_support"),
		},
		Redis: RedisConfig{
			Addr:     getEnv("REDIS_ADDR", "127.0.0.1:6379"),
			Password: os.Getenv("REDIS_PASSWORD"),
			DB:       0,
		},
		JWT: JWTConfig{
			Secret: os.Getenv("JWT_SECRET"),
			Issuer: getEnv("JWT_ISSUER", "bili-support"),
			TTL:    getEnv("JWT_TTL", "2h"),
		},
		LLM: LLMConfig{
			BaseURL: getEnv("LLM_BASE_URL", ""),
			Model:   getEnv("LLM_MODEL", ""),
			APIKey:  os.Getenv("LLM_API_KEY"),
			Timeout: getEnv("LLM_TIMEOUT", "30s"),
		},
		Elasticsearch: ElasticsearchConfig{
			URL:        getEnv("ES_URL", "http://127.0.0.1:9200"),
			IndexAlias: getEnv("ES_INDEX_ALIAS", "rule_chunks_current"),
			Timeout:    getEnv("ES_TIMEOUT", "5s"),
		},
		Embedding: EmbeddingConfig{
			URL:   getEnv("EMBEDDING_URL", "http://127.0.0.1:11434"),
			Model: getEnv("EMBEDDING_MODEL", "bge-m3:567m"),
			ModelDigest: getEnv(
				"EMBEDDING_MODEL_DIGEST",
				"7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab",
			),
			Dimensions:    getEnv("EMBEDDING_DIMENSIONS", "1024"),
			ContextLength: getEnv("EMBEDDING_CONTEXT_LENGTH", "8192"),
			Timeout:       getEnv("EMBEDDING_TIMEOUT", "60s"),
		},
		Milvus: MilvusConfig{
			URL:             getEnv("MILVUS_URL", "http://127.0.0.1:19530"),
			Database:        getEnv("MILVUS_DATABASE", "default"),
			CollectionAlias: getEnv("MILVUS_COLLECTION_ALIAS", "rule_chunks_vector_current"),
			Timeout:         getEnv("MILVUS_TIMEOUT", "5s"),
		},
		TrustedProxies: splitTrustedProxies(os.Getenv("TRUSTED_PROXIES")),
	}
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}

func splitTrustedProxies(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	return strings.Split(raw, ",")
}
