package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/retrieval"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// These framework examples preserve the existing hand-written contract tests.
// Injected identity isolates the handler; it does not prove JWT authorization.
func mockSearchRequest(t *testing.T, retriever retrieval.Retriever, ctx context.Context, query string, authenticated bool) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	r := gin.New()
	written := false
	r.Use(func(c *gin.Context) {
		if authenticated {
			c.Set("auth.user_id", uint64(99))
		}
		c.Next()
		// Capture before Gin finalizes its implicit status header.
		written = c.Writer.Written()
	})
	r.GET(searchHTTPPath, NewKnowledgeSearchHandler(retriever).Search)
	req := httptest.NewRequest(http.MethodGet, searchHTTPPath, nil).WithContext(ctx)
	req.URL.RawQuery = query
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w, written
}

func TestKnowledgeSearchMockSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	backend := NewMockRetriever(ctrl)
	type markerKey struct{}
	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), markerKey{}, "request-marker"), deadline)
	defer cancel()
	opts := retrieval.SearchOptions{DocumentID: 11, CategoryID: 41, Limit: 2, Highlight: true}
	backend.EXPECT().Retrieve(gomock.Eq(ctx), "test", gomock.Eq(opts)).DoAndReturn(
		func(actual context.Context, _ string, _ retrieval.SearchOptions) ([]retrieval.RuleChunk, error) {
			gotDeadline, ok := actual.Deadline()
			require.True(t, ok)
			assert.Equal(t, deadline, gotDeadline)
			assert.Equal(t, "request-marker", actual.Value(markerKey{}))
			return []retrieval.RuleChunk{}, nil
		},
	).Times(1)
	w, written := mockSearchRequest(t, backend, ctx, "q=+test+&document_id=11&category_id=41&limit=2&highlight=true", true)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, written)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.JSONEq(t, `{"items":[],"count":0,"limit":2}`, w.Body.String())
}

func TestKnowledgeSearchMockRejectsBeforeDependency(t *testing.T) {
	for _, tc := range []struct {
		name          string
		query         string
		authenticated bool
		status        int
		body          string
	}{
		{"unauthenticated", "q=%ZZ", false, 401, `{"code":"unauthorized","message":"authentication required"}`},
		{"invalid_query", "q=test&limit=51", true, 400, `{"code":"invalid_search_query","message":"invalid search query"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := NewMockRetriever(gomock.NewController(t))
			// No expectations: any dependency call is an unexpected-call failure.
			w, written := mockSearchRequest(t, backend, context.Background(), tc.query, tc.authenticated)
			require.Equal(t, tc.status, w.Code)
			assert.True(t, written)
			assert.JSONEq(t, tc.body, w.Body.String())
			if !tc.authenticated {
				assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestKnowledgeSearchMockDependencyError(t *testing.T) {
	backend := NewMockRetriever(gomock.NewController(t))
	ctx := context.Background()
	backend.EXPECT().Retrieve(gomock.Eq(ctx), "test", retrieval.SearchOptions{Limit: 10}).Return(
		nil, fmt.Errorf("private-backend-sentinel: %w", elasticsearch.ErrUnavailable),
	).Times(1)
	w, _ := mockSearchRequest(t, backend, ctx, "q=test", true)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"code":"search_unavailable","message":"search is temporarily unavailable"}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), "private-backend-sentinel")
}

func TestKnowledgeSearchMockCancellation(t *testing.T) {
	for _, phase := range []string{"before", "during"} {
		t.Run(phase, func(t *testing.T) {
			backend := NewMockRetriever(gomock.NewController(t))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before" {
				cancel()
			} else {
				backend.EXPECT().Retrieve(gomock.Eq(ctx), "test", retrieval.SearchOptions{Limit: 10}).DoAndReturn(
					func(context.Context, string, retrieval.SearchOptions) ([]retrieval.RuleChunk, error) {
						cancel()
						return []retrieval.RuleChunk{{Content: "late-result-sentinel"}}, nil
					},
				).Times(1)
			}
			w, written := mockSearchRequest(t, backend, ctx, "q=test", true)
			assert.False(t, written, "handler must not write a canceled result")
			assert.Empty(t, w.Body.String())
			// Recorder's default status is not evidence of successful execution.
		})
	}
}
