package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

func TestKnowledgeHistoryArticleStringIdentity(t *testing.T) {
	const (
		documentID uint64 = 9007199254740993
		versionID  uint64 = 9007199254740995
		articleID  uint64 = 9007199254740997
		categoryID uint64 = ^uint64(0)
	)
	version := &model.RuleVersion{
		ID:         versionID,
		DocumentID: documentID,
		CategoryID: categoryID,
		Version:    "test-v1",
		Title:      "虚构历史规则",
		SourceURL:  "https://www.bilibili.com/__rag_fixture__",
	}
	article := &model.RuleArticle{
		ID:            articleID,
		VersionID:     versionID,
		ArticleOrder:  1,
		ArticleNumber: "TEST-1",
		Content:       "虚构历史条款正文",
	}

	states := []struct {
		name  string
		state service.KnowledgeHistoryState
	}{
		{"current", service.KnowledgeHistoryState{
			IsCurrent: true, IsCurrentEffective: true,
		}},
		{"superseded", service.KnowledgeHistoryState{
			IsSuperseded: true,
		}},
		{"expired", service.KnowledgeHistoryState{
			IsCurrent: true, IsExpired: true,
		}},
		{"withdrawn", service.KnowledgeHistoryState{
			IsSuperseded: true, IsWithdrawn: true,
		}},
		{"no_current_effective_state", service.KnowledgeHistoryState{}},
	}

	var currentState service.KnowledgeHistoryState
	calls := 0
	business := &revisionHistoryBusiness{
		article: func(
			ctx context.Context, gotVersionID, gotArticleID uint64,
		) (*service.KnowledgeHistoryArticleDetail, error) {
			calls++
			revisionHistoryCheckContext(t, ctx)
			if gotVersionID != versionID || gotArticleID != articleID {
				t.Fatalf("IDs changed: %d/%d", gotVersionID, gotArticleID)
			}
			return &service.KnowledgeHistoryArticleDetail{
				Version: version,
				Article: article,
				State:   currentState,
			}, nil
		},
	}
	router, token := revisionHistoryRouter(t, model.RoleUser, business)
	path := fmt.Sprintf(
		"/api/v1/knowledge/versions/%d/articles/%d", versionID, articleID,
	)

	for i, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			currentState = tc.state
			recorder := managementRequest(
				router, http.MethodGet, path, "", token, false,
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("historical article state must not be cached")
			}

			var body struct {
				Identity map[string]string `json:"identity"`
				Version  struct {
					DocumentID uint64 `json:"document_id"`
					VersionID  uint64 `json:"version_id"`
					CategoryID uint64 `json:"category_id"`
					Title      string `json:"title"`
				} `json:"version"`
				Article struct {
					ArticleID     uint64 `json:"article_id"`
					ArticleNumber string `json:"article_number"`
					Content       string `json:"content"`
				} `json:"article"`
				State map[string]bool `json:"state"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantIdentity := map[string]string{
				"document_id": "9007199254740993",
				"version_id":  "9007199254740995",
				"article_id":  "9007199254740997",
				"category_id": "18446744073709551615",
			}
			if !reflect.DeepEqual(body.Identity, wantIdentity) {
				t.Fatalf("string identity changed: %+v", body.Identity)
			}
			if body.Version.DocumentID != documentID ||
				body.Version.VersionID != versionID ||
				body.Version.CategoryID != categoryID ||
				body.Version.Title != version.Title ||
				body.Article.ArticleID != articleID ||
				body.Article.ArticleNumber != article.ArticleNumber ||
				body.Article.Content != article.Content {
				t.Fatal("legacy numeric fields or historical content changed")
			}
			wantState := map[string]bool{
				"is_current":           tc.state.IsCurrent,
				"is_superseded":        tc.state.IsSuperseded,
				"is_expired":           tc.state.IsExpired,
				"is_withdrawn":         tc.state.IsWithdrawn,
				"is_current_effective": tc.state.IsCurrentEffective,
			}
			if !reflect.DeepEqual(body.State, wantState) {
				t.Fatalf("state=%v want=%v", body.State, wantState)
			}
			if calls != i+1 {
				t.Fatalf("query count=%d want=%d", calls, i+1)
			}
		})
	}
}
