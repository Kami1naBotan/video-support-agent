package handler_test

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/service"
)

type deliveryBusiness struct {
	handler.KnowledgeService
	calls int
	fail  error
}

func (s *deliveryBusiness) GetDocumentDeliveries(_ context.Context, id, before uint64, limit int) (*service.KnowledgeDeliveryPage, error) {
	s.calls++
	if s.fail != nil {
		return nil, s.fail
	}
	return &service.KnowledgeDeliveryPage{DocumentID: id, Items: []service.KnowledgeDelivery{{ID: uint64(limit), Attempts: before, Status: "pending"}}}, nil
}
func TestDeliveryHTTPContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name, path string
		fail       error
		want       int
	}{
		{"default", "/documents/7/deliveries", nil, 200},
		{"paging", "/documents/7/deliveries?limit=100&before=9", nil, 200},
		{"zero_id", "/documents/0/deliveries", nil, 400},
		{"leading_zero", "/documents/07/deliveries", nil, 400},
		{"overflow_id", "/documents/18446744073709551616/deliveries", nil, 400},
		{"limit_empty", "/documents/7/deliveries?limit=", nil, 400},
		{"limit_zero", "/documents/7/deliveries?limit=0", nil, 400},
		{"limit_large", "/documents/7/deliveries?limit=101", nil, 400},
		{"duplicate", "/documents/7/deliveries?limit=2&limit=3", nil, 400},
		{"unknown", "/documents/7/deliveries?payload=true", nil, 400},
		{"cursor_zero", "/documents/7/deliveries?before=0", nil, 400},
		{"cursor_negative", "/documents/7/deliveries?before=-1", nil, 400},
		{"bad_escape", "/documents/7/deliveries?limit=%zz", nil, 400},
		{"not_found", "/documents/7/deliveries", service.ErrKnowledgeDocumentNotFound, 404},
		{"internal", "/documents/7/deliveries", errors.New("DATABASE_SECRET"), 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			business := &deliveryBusiness{fail: tc.fail}
			r := gin.New()
			r.GET("/documents/:document_id/deliveries", handler.NewKnowledgeHandler(business).GetDocumentDeliveries)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want == 400 && business.calls != 0 {
				t.Fatal("invalid query reached service")
			}
			if tc.want != 400 && business.calls != 1 {
				t.Fatal("service not called once")
			}
			raw := w.Body.String()
			if strings.Contains(raw, "DATABASE_SECRET") {
				t.Fatal("error leaked")
			}
			if tc.name == "default" && !strings.Contains(raw, `"id":"20"`) {
				t.Fatal("default limit lost")
			}
			if tc.name == "paging" && (!strings.Contains(raw, `"id":"100"`) || !strings.Contains(raw, `"attempts":"9"`)) {
				t.Fatal("pagination args lost")
			}
		})
	}
}
