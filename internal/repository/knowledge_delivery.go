package repository

import (
	"context"
	"errors"
	"strconv"
	"time"

	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"gorm.io/gorm"
)

var _ service.KnowledgeDeliveryReader = (*KnowledgeRepository)(nil)

func (r *KnowledgeRepository) ReadDocumentDeliveries(ctx context.Context, documentID uint64, before uint64, limit int) ([]service.KnowledgeDelivery, error) {
	if documentID == 0 || limit < 1 || limit > 100 {
		return nil, service.ErrInvalidArgument
	}

	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var document model.KnowledgeDocument
	err := r.db.WithContext(bounded).Select("id").Where("id = ?", documentID).Take(&document).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, service.ErrKnowledgeDocumentNotFound
	}
	if err != nil {
		return nil, errors.New("delivery_document_lookup_failed")
	}

	query := r.db.WithContext(bounded).
		Model(&model.EventOutbox{}).
		Select(
			"id",
			"message_id",
			"event_id",
			"trace_id",
			"status",
			"attempts",
			"safe_error_code",
			"next_attempt_at",
			"sent_at",
			"created_at",
			"updated_at",
		).
		Where(
			"topic = ? AND message_key = ?",
			eventbus.TopicDocumentEvents,
			strconv.FormatUint(documentID, 10),
		)

	if before != 0 {
		query = query.Where("id < ?", before)
	}

	rows := make([]service.KnowledgeDelivery, 0, limit+1)
	err = query.Order("id DESC").Limit(limit + 1).Scan(&rows).Error
	if err != nil {
		return nil, errors.New("delivery_list_failed")
	}

	return rows, nil
}
