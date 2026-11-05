package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"

	"gorm.io/gorm"
)

func readWorkerDefinition(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("worker_definition_unavailable")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("worker_definition_invalid")
	}

	const maxBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxBytes {
		return nil, errors.New("worker_definition_invalid")
	}

	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 2 {
		return nil, errors.New("worker_definition_invalid")
	}
	for _, name := range []string{"settings", "mappings"} {
		var object map[string]json.RawMessage
		value, ok := fields[name]
		if !ok || json.Unmarshal(value, &object) != nil || object == nil {
			return nil, errors.New("worker_definition_invalid")
		}
	}
	return data, nil
}

func checkKnowledgeWorkerSchema(ctx context.Context, db *gorm.DB) error {
	models := []any{
		&model.EventOutbox{},
		&model.KnowledgeIndexControl{},
		&model.KnowledgeIndexBootstrap{},
		&model.KnowledgeEventInbox{},
		&model.KnowledgeIndexAttempt{},
		&model.KnowledgeIndexTask{},
		&model.KnowledgeIndexGeneration{},
		&model.KnowledgeDocumentIndex{},
		&model.KnowledgeBuildRecovery{},
		&model.KnowledgeBuildEvent{},
		&model.KnowledgeRetryReceipt{},
		&model.KnowledgePoison{},
		&model.KnowledgeDocument{},
		&model.RuleVersion{},
		&model.RuleArticle{},
		&model.KnowledgeChunk{},
	}

	for _, value := range models {
		statement := &gorm.Statement{DB: db}
		if err := statement.Parse(value); err != nil {
			return errors.New("worker_schema_invalid")
		}

		rows, err := db.WithContext(ctx).Model(value).Select(statement.Schema.DBNames).Where("1 = 0").Rows()
		if err != nil {
			return errors.New("worker_schema_invalid")
		}
		if err := rows.Close(); err != nil {
			return errors.New("worker_schema_invalid")
		}
	}
	return nil
}

func checkWorkerSource(ctx context.Context, db *gorm.DB, cfg config.Config, settings config.KnowledgeWorkerSettings) error {
	var identity struct {
		DatabaseName string
		ServerUUID   string
	}
	if err := db.WithContext(ctx).
		Raw("SELECT DATABASE() AS database_name, @@server_uuid AS server_uuid").
		Scan(&identity).Error; err != nil {
		return errors.New("worker_source_check_failed")
	}
	if identity.DatabaseName != cfg.Database.Name || identity.ServerUUID != settings.ExpectedServerUUID {
		return errors.New("worker_source_identity_mismatch")
	}

	var control model.KnowledgeIndexControl
	if err := db.WithContext(ctx).Where("id = ?", model.KnowledgeIndexControlID).Take(&control).Error; err != nil {
		return errors.New("worker_control_missing")
	}
	if control.DesiredEpoch == 0 ||
		control.ActiveEpoch > control.DesiredEpoch ||
		(control.ActiveEpoch == 0 && control.ActiveGenerationID != nil) ||
		(control.ActiveEpoch > 0 && control.ActiveGenerationID == nil) ||
		(control.OwnerToken == nil) != (control.LeaseUntil == nil) ||
		(control.GenerationLimit != 0 && control.GenerationLimit != settings.GenerationLimit) {
		return errors.New("worker_control_invalid")
	}

	var bootstrap model.KnowledgeIndexBootstrap
	if err := db.WithContext(ctx).
		Where("id = ?", model.KnowledgeIndexControlID).
		Take(&bootstrap).Error; err != nil {
		return errors.New("worker_bootstrap_missing")
	}
	switch bootstrap.Status {
	case "pending", "scheduled", "completed":
		return nil
	default:
		return errors.New("worker_bootstrap_invalid")
	}
}
