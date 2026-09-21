package config

import "os"

type Config struct {
	AppName  string
	Env      string
	Port     string
	LogLevel string
}

func Load() Config {
	return Config{
		AppName:  getEnv("APP_NAME", "bilibili-support-agent-api"),
		Env:      getEnv("APP_ENV", "development"),
		Port:     getEnv("APP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
	}
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}
