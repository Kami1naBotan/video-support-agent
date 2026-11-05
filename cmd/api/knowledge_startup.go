package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var knowledgeSourceUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func checkKnowledgeAPIStartup(ctx context.Context, db *gorm.DB, cfg config.Config, expectedServerUUID string) (bool, error) {
	if db == nil || cfg.Database.Name == "" || !knowledgeSourceUUIDPattern.MatchString(expectedServerUUID) {
		return false, errors.New("api_knowledge_startup_config_invalid")
	}
	if err := config.ValidateEmbeddingConfig(cfg.Embedding); err != nil {
		return false, errors.New("api_knowledge_embedding_config_invalid")
	}

	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	checked := db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).WithContext(bounded)

	if err := checkKnowledgeAPISchema(bounded, checked); err != nil {
		return false, err
	}

	var identity struct {
		DatabaseName string
		ServerUUID   string
	}
	if err := checked.Raw("SELECT DATABASE() AS database_name, @@server_uuid AS server_uuid").Scan(&identity).Error; err != nil {
		return false, errors.New("api_knowledge_source_check_failed")
	}
	if identity.DatabaseName != cfg.Database.Name || identity.ServerUUID != expectedServerUUID {
		return false, errors.New("api_knowledge_source_identity_mismatch")
	}

	return checkKnowledgeAPIState(bounded, checked, cfg, expectedServerUUID)
}

func checkKnowledgeAPISchema(ctx context.Context, db *gorm.DB) error {
	models := []any{
		&model.RuleCategory{},
		&model.KnowledgeDocument{},
		&model.RuleVersion{},
		&model.RuleArticle{},
		&model.KnowledgeChunk{},
		&model.RuleVerification{},
		&model.RuleOperation{},
		&model.EventOutbox{},
		&model.KnowledgeIndexControl{},
		&model.KnowledgeIndexBootstrap{},
		&model.KnowledgeIndexGeneration{},
	}

	for _, value := range models {
		if err := ctx.Err(); err != nil {
			return err
		}

		statement := &gorm.Statement{DB: db}
		if err := statement.Parse(value); err != nil {
			return errors.New("api_knowledge_schema_invalid")
		}

		rows, err := db.WithContext(ctx).Model(value).Select(statement.Schema.DBNames).Where("1 = 0").Rows()
		if err != nil {
			return errors.New("api_knowledge_schema_invalid")
		}
		if err := rows.Close(); err != nil {
			return errors.New("api_knowledge_schema_invalid")
		}
	}
	return nil
}

func checkKnowledgeAPIState(ctx context.Context, db *gorm.DB, cfg config.Config, expectedServerUUID string) (bool, error) {
	ready := false
	checkFailed := errors.New("api_knowledge_state_check_failed")

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var control model.KnowledgeIndexControl
		if err := tx.Where("id = ?", model.KnowledgeIndexControlID).Take(&control).Error; err != nil {
			return checkFailed
		}

		if control.DesiredEpoch == 0 ||
			control.ActiveEpoch > control.DesiredEpoch ||
			(control.ActiveEpoch == 0 && control.ActiveGenerationID != nil) ||
			(control.ActiveEpoch > 0 && control.ActiveGenerationID == nil) ||
			(control.OwnerToken == nil) != (control.LeaseUntil == nil) {
			return checkFailed
		}
		if control.OwnerToken != nil && *control.OwnerToken == "" {
			return checkFailed
		}

		var bootstrap model.KnowledgeIndexBootstrap
		if err := tx.Where("id = ?", model.KnowledgeIndexControlID).Take(&bootstrap).Error; err != nil {
			return checkFailed
		}

		switch bootstrap.Status {
		case "pending", "scheduled":
			if bootstrap.CompletedAt != nil || control.ActiveEpoch != 0 {
				return checkFailed
			}
		case "completed":
			if bootstrap.CompletedAt == nil || bootstrap.CompletedAt.IsZero() || control.ActiveEpoch == 0 {
				return checkFailed
			}
		default:
			return checkFailed
		}

		if control.ActiveEpoch == 0 {
			return nil
		}

		if err := checkKnowledgeAPIActive(ctx, tx, cfg, expectedServerUUID); err != nil {
			return checkFailed
		}

		ready = true
		return nil
	}, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})

	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err != nil {
		return false, checkFailed
	}
	return ready, nil
}

func checkKnowledgeAPIActive(ctx context.Context, tx *gorm.DB, cfg config.Config, expectedServerUUID string) error {
	activeRepository, err := repository.NewKnowledgeActiveRepository(tx)
	if err != nil {
		return err
	}

	target, err := activeRepository.ResolveActive(ctx)
	if err != nil {
		return err
	}
	if target.Identity.Digest != cfg.Embedding.ModelDigest {
		return errors.New("api_knowledge_active_model_mismatch")
	}

	var generation model.KnowledgeIndexGeneration
	if err := tx.
		Select("id", "source_database", "source_server_uuid").
		Where("id = ?", target.GenerationID).
		Take(&generation).Error; err != nil {
		return errors.New("api_knowledge_active_source_unavailable")
	}

	if generation.SourceDatabase == nil ||
		generation.SourceServerUUID == nil ||
		*generation.SourceDatabase != cfg.Database.Name ||
		*generation.SourceServerUUID != expectedServerUUID {
		return errors.New("api_knowledge_active_source_mismatch")
	}

	return nil
}
