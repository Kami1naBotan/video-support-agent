package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

type deliveryStore struct {
	service.KnowledgeStore
	read func(context.Context, uint64, uint64, int) ([]service.KnowledgeDelivery, error)
}

func (s deliveryStore) ReadDocumentDeliveries(c context.Context, id, before uint64, limit int) ([]service.KnowledgeDelivery, error) {
	return s.read(c, id, before, limit)
}
func TestDeliveryPageContract(t *testing.T) {
	now := time.Now().UTC()
	calls := 0
	s := service.NewKnowledgeService(deliveryStore{read: func(ctx context.Context, id, before uint64, limit int) ([]service.KnowledgeDelivery, error) {
		calls++
		if id != 7 || before != 10 || limit != 2 {
			t.Fatal("wrong query")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded read")
		}
		return []service.KnowledgeDelivery{{ID: 9, Status: model.OutboxSent, SafeErrorCode: "RAW_SECRET", NextAttemptAt: &now, SentAt: &now}, {ID: 8, Status: model.OutboxPending, SentAt: &now, NextAttemptAt: &now}, {ID: 7, Status: model.OutboxBlocked}}, nil
	}})
	page, err := s.GetDocumentDeliveries(context.Background(), 7, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextBefore != "8" || page.DocumentID != 7 || calls != 1 {
		t.Fatalf("page: %+v", page)
	}
	if page.Items[0].SafeErrorCode != "unclassified" || page.Items[0].NextAttemptAt != nil || page.Items[0].SentAt == nil || page.Items[1].SentAt != nil || page.Items[1].NextAttemptAt == nil {
		t.Fatal("unsafe status projection")
	}
	b, _ := json.Marshal(page)
	raw := string(b)
	if !strings.Contains(raw, `"id":"9"`) || !strings.Contains(raw, `"document_id":"7"`) || strings.Contains(raw, "RAW_SECRET") {
		t.Fatal(raw)
	}
	for _, name := range []string{"empty", "overflow", "order", "cursor", "status", "missing"} {
		t.Run(name, func(t *testing.T) {
			store := deliveryStore{read: func(context.Context, uint64, uint64, int) ([]service.KnowledgeDelivery, error) {
				switch name {
				case "empty":
					return nil, nil
				case "overflow":
					return make([]service.KnowledgeDelivery, 4), nil
				case "order":
					return []service.KnowledgeDelivery{{ID: 1, Status: "sent"}, {ID: 2, Status: "sent"}}, nil
				case "cursor":
					return []service.KnowledgeDelivery{{ID: 10, Status: "sent"}}, nil
				case "status":
					return []service.KnowledgeDelivery{{ID: 1, Status: "ready"}}, nil
				default:
					return nil, service.ErrKnowledgeDocumentNotFound
				}
			}}
			p, e := service.NewKnowledgeService(store).GetDocumentDeliveries(context.Background(), 7, 10, 2)
			if name == "empty" {
				if e != nil || p.Items == nil || len(p.Items) != 0 || p.NextBefore != "" {
					t.Fatal("empty contract")
				}
			} else if e == nil {
				t.Fatal("bad result accepted")
			}
			if name == "missing" && !errors.Is(e, service.ErrKnowledgeDocumentNotFound) {
				t.Fatal("lost not found")
			}
		})
	}
	for _, args := range []struct {
		id    uint64
		limit int
	}{{0, 20}, {1, 0}, {1, 101}} {
		_, err := s.GetDocumentDeliveries(context.Background(), args.id, 0, args.limit)
		if !errors.Is(err, service.ErrInvalidArgument) {
			t.Fatal("bad argument accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid inputs reached store")
	}
}
