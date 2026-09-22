package database

import (
	"context"
	"fmt"
	"net"
	"time"

	"video-support-agent/internal/config"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func OpenMySQL(cfg config.DatabaseConfig) (*gorm.DB, error) {
	if cfg.Password == "" {
		return nil, fmt.Errorf("DB_PASSWORD is required")
	}

	dsn := driver.NewConfig()
	dsn.User = cfg.User
	dsn.Passwd = cfg.Password
	dsn.Net = "tcp"
	dsn.Addr = net.JoinHostPort(cfg.Host, cfg.Port)
	dsn.DBName = cfg.Name
	dsn.ParseTime = true
	dsn.Loc = time.UTC
	dsn.Timeout = 5 * time.Second

	db, err := gorm.Open(mysql.Open(dsn.FormatDSN()), &gorm.Config{})

	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get mysql connection pool: %w", err)
	}

	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}

	return db, nil
}
