//go:build searchintegration

package searchintegration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/auth"
	"video-support-agent/internal/cache"
	"video-support-agent/internal/config"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/router"
)

type candidateFunc func(
	context.Context, string, retrieval.SearchOptions, time.Time,
) ([]retrieval.Candidate, error)

func (f candidateFunc) SearchCandidates(
	ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time,
) ([]retrieval.Candidate, error) {
	return f(ctx, query, opts, at)
}

type limiterFunc func(context.Context, uint64) (cache.RateLimitResult, error)

func (f limiterFunc) Allow(ctx context.Context, id uint64) (cache.RateLimitResult, error) {
	return f(ctx, id)
}

func TestSearchMySQLRouter(t *testing.T) {
	s := newSearchSuite(t)
	candidate := s.fixture(t)
	user := model.User{
		Username:     "isolated_search_user",
		PasswordHash: "not-a-login-password",
		Role:         model.RoleUser,
	}
	must(t, s.db.Create(&user).Error)

	tokens, err := auth.NewTokenManager(
		strings.Repeat("s", 32), "isolated-search", time.Hour)
	must(t, err)
	token, err := tokens.Issue(user.ID)
	must(t, err)

	searchCalls, limitCalls := 0, 0
	mode := "allow"
	var queryDeadline time.Time
	var expectedParent time.Time
	searcher := candidateFunc(func(
		ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time,
	) ([]retrieval.Candidate, error) {
		searchCalls++
		if query != "test" || opts.Limit != 10 || !at.Equal(s.at) {
			return nil, errors.New("搜索参数或共享时刻错误")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 40*time.Second {
			return nil, errors.New("实际路由未传递有效的40秒以内截止时间")
		}
		if !expectedParent.IsZero() && !deadline.Equal(expectedParent) {
			return nil, errors.New("较早父截止时间丢失")
		}
		queryDeadline = deadline
		return []retrieval.Candidate{candidate}, nil
	})
	searchService, err := retrieval.NewServiceWithClock(
		searcher, repository.NewKnowledgeRepository(s.db),
		func() time.Time { return s.at },
	)
	must(t, err)
	limiter := limiterFunc(func(ctx context.Context, id uint64) (cache.RateLimitResult, error) {
		limitCalls++
		deadline, ok := ctx.Deadline()
		if id != user.ID || !ok || time.Until(deadline) > 500*time.Millisecond {
			return cache.RateLimitResult{}, errors.New("限流身份或截止时间错误")
		}
		switch mode {
		case "deny":
			return cache.RateLimitResult{Allowed: false, RetryAfter: 1500 * time.Millisecond}, nil
		case "error":
			return cache.RateLimitResult{}, errors.New("test limiter unavailable")
		default:
			return cache.RateLimitResult{Allowed: true}, nil
		}
	})
	cfg := config.Config{AppName: "isolated-search", Env: "test"}
	r := router.NewWithRetriever(
		cfg, s.db, tokens, nil, limiter, nil, nil, nil, searchService,
	)

	request := func(t *testing.T, handler http.Handler, authenticated bool,
		ctx context.Context, status int, code string, wantItems int, wantSearch int) *httptest.ResponseRecorder {
		t.Helper()
		before := searchCalls
		req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/search?q=test", nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if ctx != nil {
			req = req.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status || searchCalls-before != wantSearch {
			t.Fatalf("status=%d body=%s searchCalls=%d",
				w.Code, w.Body.String(), searchCalls-before)
		}
		var body struct {
			Code  string `json:"code"`
			Count int    `json:"count"`
			Limit int    `json:"limit"`
			Items []struct {
				ChunkID uint64 `json:"chunk_id"`
			} `json:"items"`
		}
		must(t, json.Unmarshal(w.Body.Bytes(), &body))
		if status == 200 {
			if body.Code != "" || body.Items == nil ||
				body.Count != wantItems || len(body.Items) != wantItems || body.Limit != 10 {
				t.Fatalf("搜索成功响应不符：%s", w.Body.String())
			}
			if wantItems == 1 && body.Items[0].ChunkID != candidate.Chunk.ChunkID {
				t.Fatalf("返回了错误片段：%s", w.Body.String())
			}
		} else if body.Code != code || body.Items != nil {
			t.Fatalf("错误响应不符：%s", w.Body.String())
		}
		return w
	}

	runCase(t, "missing_token_stops_before_limiter", func(t *testing.T) {
		before := limitCalls
		w := request(t, r, false, nil, 401, "unauthorized", 0, 0)
		if limitCalls != before || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatal("未登录请求没有在JWT阶段结束")
		}
	})

	// 同一枚JWT在数据库角色改变后重复使用，证明角色来自当前数据库。
	for _, role := range []string{model.RoleUser, model.RoleSupport, model.RoleAdmin} {
		runCase(t, "database_role_"+role, func(t *testing.T) {
			must(t, s.db.Model(&model.User{}).
				Where("id = ?", user.ID).Update("role", role).Error)
			queryDeadline = time.Time{}
			request(t, r, true, nil, 200, "", 1, 1)
			if queryDeadline.IsZero() {
				t.Fatal("搜索上下文未被观测")
			}
		})
	}

	runCase(t, "unknown_database_role", func(t *testing.T) {
		must(t, s.db.Model(&model.User{}).
			Where("id = ?", user.ID).Update("role", "unknown").Error)
		request(t, r, true, nil, 403, "forbidden", 0, 0)
		must(t, s.db.Model(&model.User{}).
			Where("id = ?", user.ID).Update("role", model.RoleUser).Error)
	})

	runCase(t, "earlier_parent_deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		expectedParent, _ = ctx.Deadline()
		defer func() { expectedParent = time.Time{} }()
		request(t, r, true, ctx, 200, "", 1, 1)
	})

	runCase(t, "rate_limited", func(t *testing.T) {
		mode = "deny"
		defer func() { mode = "allow" }()
		w := request(t, r, true, nil, 429, "rate_limited", 0, 0)
		if w.Header().Get("Retry-After") != "2" {
			t.Fatalf("Retry-After=%q，预期2", w.Header().Get("Retry-After"))
		}
	})

	runCase(t, "limiter_failure", func(t *testing.T) {
		mode = "error"
		defer func() { mode = "allow" }()
		request(t, r, true, nil, 503, "rate_limit_unavailable", 0, 0)
	})

	runCase(t, "compatibility_constructor_without_retriever", func(t *testing.T) {
		legacy := router.New(cfg, s.db, tokens, nil, limiter, nil, nil, nil)
		request(t, legacy, true, nil, 500, "internal_error", 0, 0)
	})

	runCase(t, "stale_candidate_filtered_by_real_mysql", func(t *testing.T) {
		must(t, s.db.Model(&model.KnowledgeDocument{}).
			Where("id = ?", candidate.Chunk.DocumentID).
			Update("publication_revision", 2).Error)
		request(t, r, true, nil, 200, "", 0, 1)
		candidate.PublicationRevision = 2
		request(t, r, true, nil, 200, "", 1, 1)
	})

	runCase(t, "verification_database_failure", func(t *testing.T) {
		must(t, s.db.Exec("RENAME TABLE knowledge_chunks TO search_hidden_chunks").Error)
		t.Cleanup(func() {
			must(t, s.db.Exec("RENAME TABLE search_hidden_chunks TO knowledge_chunks").Error)
		})
		request(t, r, true, nil, 503, "search_verification_failed", 0, 1)
	})

	runCase(t, "deleted_user_with_existing_jwt", func(t *testing.T) {
		must(t, s.db.Delete(&model.User{}, user.ID).Error)
		request(t, r, true, nil, 401, "unauthorized", 0, 0)
	})

	t.Log("REAL_JWT_DATABASE_ROLES_AND_SEARCH_VERIFICATION=true")
	t.Log("LIMITER_BACKEND=stub ES_CANDIDATES=stub")
}
