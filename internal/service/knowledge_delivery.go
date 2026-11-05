package service

import (
	"context"
	"errors"
	"strconv"
	"time"
	"video-support-agent/internal/model"
)

type KnowledgeDelivery struct {
	ID            uint64     `json:"id,string"`
	MessageID     string     `json:"message_id"`
	EventID       string     `json:"event_id"`
	TraceID       string     `json:"trace_id"`
	Status        string     `json:"status"`
	Attempts      uint64     `json:"attempts,string"`
	SafeErrorCode string     `json:"safe_error_code,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type KnowledgeDeliveryPage struct {
	DocumentID uint64              `json:"document_id,string"`
	Items      []KnowledgeDelivery `json:"items"`
	NextBefore string              `json:"next_before,omitempty"`
}

type KnowledgeDeliveryReader interface {
	ReadDocumentDeliveries(context.Context, uint64, uint64, int) ([]KnowledgeDelivery, error)
}

func (s *KnowledgeService) GetDocumentDeliveries(ctx context.Context, documentID uint64, before uint64, limit int) (*KnowledgeDeliveryPage, error) {
	if documentID == 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalidArgument
	}

	reader, ok := s.store.(KnowledgeDeliveryReader)
	if !ok {
		return nil, errors.New("knowledge delivery reader is not configured")
	}

	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := reader.ReadDocumentDeliveries(bounded, documentID, before, limit)
	if err != nil {
		return nil, err
	}
	if len(rows) > limit+1 {
		return nil, errors.New("knowledge delivery reader exceeded limit")
	}

	page := &KnowledgeDeliveryPage{
		DocumentID: documentID,
		Items:      make([]KnowledgeDelivery, 0, limit),
	}

	if len(rows) > limit {
		page.NextBefore = strconv.FormatUint(rows[limit-1].ID, 10)
		rows = rows[:limit]
	}

	var previous uint64
	for i, row := range rows {
		if row.ID == 0 || (before != 0 && row.ID >= before) || (i > 0 && row.ID >= previous) {
			return nil, errors.New("knowledge delivery order is invalid")
		}
		previous = row.ID

		switch row.Status {
		case model.OutboxPending, model.OutboxSending, model.OutboxSent, model.OutboxBlocked:
		default:
			return nil, errors.New("knowledge delivery status is invalid")
		}

		switch row.SafeErrorCode {
		case "", "delivery_unknown", "not_submitted", "invalid_record", "payload_mismatch", "attempts_exhausted":
		default:
			row.SafeErrorCode = "unclassified"
		}

		if row.Status != model.OutboxPending {
			row.NextAttemptAt = nil
		}
		if row.Status != model.OutboxSent {
			row.SentAt = nil
		}

		page.Items = append(page.Items, row)
	}

	return page, nil
}
