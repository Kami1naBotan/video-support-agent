package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/middleware"

	"github.com/gin-gonic/gin"
)

func TestBodyLimitBinding(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		unknownSize bool
		wantStatus  int
	}{
		{
			name:       "valid JSON",
			body:       `{"content":"hello"}`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "invalid JSON",
			body:       `{"content":`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "known oversized body",
			body:       `{"content":"` + strings.Repeat("x", 128) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name:        "unknown oversized body",
			body:        `{"content":"` + strings.Repeat("x", 128) + `"}`,
			unknownSize: true,
			wantStatus:  http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.Use(middleware.BodyLimit(64))
			r.POST("/", func(c *gin.Context) {
				var request struct {
					Content string `json:"content"`
				}

				if err := c.ShouldBindJSON(&request); err != nil {
					writeBindError(c, err)
					return
				}

				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequest(
				http.MethodPost,
				"/",
				strings.NewReader(tt.body),
			)
			req.Header.Set("Content-Type", "application/json")

			if tt.unknownSize {
				req.ContentLength = -1
			}

			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf(
					"expected status %d, got %d: %s",
					tt.wantStatus,
					recorder.Code,
					recorder.Body.String(),
				)
			}
		})
	}
}

func TestRequestDeadlineMapsToGatewayTimeout(t *testing.T) {
	r := gin.New()
	r.Use(middleware.RequestDeadline(10 * time.Millisecond))

	r.GET("/", func(c *gin.Context) {
		<-c.Request.Context().Done()

		err := c.Request.Context().Err()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected deadline exceeded, got %v", err)
		}

		writeError(c, err)
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d", recorder.Code)
	}
}
