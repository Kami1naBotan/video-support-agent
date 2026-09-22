package main

import (
	"fmt"
	"log"
	"video-support-agent/internal/config"
	"video-support-agent/internal/database"
)

func run() error {
	cfg := config.Load()

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

	return database.Migrate(db)
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}

	log.Println("database migration completed")
}
