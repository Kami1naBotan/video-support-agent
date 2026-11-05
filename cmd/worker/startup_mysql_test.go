//go:build workermysql

package main

import (
	"context"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"testing"
	"time"
)

func TestWorkerSchemaMySQL(t *testing.T) {
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_HOST") != "127.0.0.1" || os.Getenv("DB_NAME") != "bili_support_test" || os.Getenv("TEST_MYSQL_SERVER_UUID") == "" {
		t.Fatal("isolated script required")
	}
	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	cfg.User = os.Getenv("DB_USER")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.DBName = os.Getenv("DB_NAME")
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("database open failed")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var identity struct{ Name, UUID string }
	if err = db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error; err != nil {
		t.Fatal(err)
	}
	if identity.Name != "bili_support_test" || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("identity mismatch")
	}
	ctx := context.Background()
	if err = checkKnowledgeWorkerSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("RENAME TABLE knowledge_poisons TO isolated_missing_poisons").Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Exec("RENAME TABLE isolated_missing_poisons TO knowledge_poisons").Error; err != nil {
			t.Error(err)
		}
	}()
	if err = checkKnowledgeWorkerSchema(ctx, db); err == nil || err.Error() != "worker_schema_invalid" {
		t.Fatalf("missing table check=%v", err)
	}
}
