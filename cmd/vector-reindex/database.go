package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"video-support-agent/internal/config"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func validateDatabaseConfig(cfg config.DatabaseConfig, expectedDatabase string) error {
	if strings.TrimSpace(cfg.Host) == "" || strings.TrimSpace(cfg.User) == "" || cfg.Password == "" || strings.TrimSpace(cfg.Name) == "" {
		return errors.New("database configuration is incomplete")
	}
	port, err := strconv.Atoi(cfg.Port)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid database port")
	}
	if cfg.Name != expectedDatabase {
		return errors.New("configured database differs from expected database")
	}
	return nil
}

func openSourceDatabase(ctx context.Context, cfg config.DatabaseConfig) (*gorm.DB, *sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	settings := driver.NewConfig()
	settings.User = cfg.User
	settings.Passwd = cfg.Password
	settings.Net = "tcp"
	settings.Addr = net.JoinHostPort(cfg.Host, cfg.Port)
	settings.DBName = cfg.Name
	settings.ParseTime = true
	settings.Loc = time.UTC
	settings.Timeout = 5 * time.Second
	settings.ReadTimeout = 5 * time.Second
	settings.WriteTimeout = 5 * time.Second
	settings.Logger = &driver.NopLogger{}

	connector, err := driver.NewConnector(settings)
	if err != nil {
		return nil, nil, err
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(30 * time.Minute)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, nil, err
	}

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		_ = pool.Close()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = pool.Close()
		return nil, nil, err
	}
	return db, pool, nil
}
