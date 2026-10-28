package main

import (
	"context"
	"fmt"
	"log"
	"time"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
)

func run() error {
	cfg := config.Load()

	log.Printf(
		"initializing rule categories: host=%s port=%s database=%s",
		cfg.Database.Host,
		cfg.Database.Port,
		cfg.Database.Name,
	)

	db, err := database.OpenMySQL(cfg.Database)
	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get database connection pool: %w", err)
	}
	defer func() {
		_ = sqlDB.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	return database.SeedRuleCategories(ctx, db)
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}

	log.Println("rule category initialization completed")
}
