package config

import "os"

type Config struct {
	AppName  string
	Env      string
	Port     string
	LogLevel string
	Database DatabaseConfig
}

type DatabaseConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
}

func Load() Config {
	return Config{
		AppName:  getEnv("APP_NAME", "bilibili-support-agent-api"),
		Env:      getEnv("APP_ENV", "development"),
		Port:     getEnv("APP_PORT", "8080"),
		LogLevel: getEnv("LOG_LEVEL", "info"),
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "3307"),
			User:     getEnv("DB_USER", "bili_app"),
			Password: os.Getenv("DB_PASSWORD"),
			Name:     getEnv("DB_NAME", "bili_support"),
		},
	}
}

func getEnv(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}

	return value
}
