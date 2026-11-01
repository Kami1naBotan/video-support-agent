//go:build indexexportintegration && vectorhydrationintegration

package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func vhMust(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	message := err.Error()
	if password := os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"); password != "" {
		message = strings.ReplaceAll(message, password, "<redacted>")
	}
	t.Fatal(message)
}

func vhSuite(t *testing.T) *exportMySQLSuite {
	t.Helper()

	if os.Getenv("RUN_VECTOR_HYDRATION_MYSQL") != "1" ||
		os.Getenv("INDEX_EXPORT_MYSQL_ADDR") != "127.0.0.1:13308" ||
		os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID") !=
			"9ae2c7f4-d375-11f1-8945-ce027cf8cce1" ||
		os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD") == "" {
		t.Fatal("explicit isolated MySQL configuration is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	_, admin := exportMySQLOpen(t, "")
	var uuid, version string
	vhMust(t, admin.QueryRowContext(ctx,
		"SELECT @@server_uuid, VERSION()",
	).Scan(&uuid, &version))
	if uuid != os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID") ||
		!strings.HasPrefix(version, "8.4.") {
		t.Fatal("isolated server identity differs; no database created")
	}

	var token [8]byte
	_, err := rand.Read(token[:])
	vhMust(t, err)
	name := "vec_hydrate_it_" + hex.EncodeToString(token[:])

	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+name+
			"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	vhMust(t, err)

	t.Logf("TEST_DATABASE=%s SERVER_UUID=%s MYSQL_VERSION=%s",
		name, uuid, version)
	t.Cleanup(func() {
		t.Logf("RESOURCES_RETAINED=true TEST_DATABASE=%s", name)
	})

	db, pool := exportMySQLOpen(t, name)
	writer, _ := exportMySQLOpen(t, name)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	db = db.WithContext(ctx)
	writer = writer.WithContext(ctx)

	vhMust(t, db.Set("gorm:table_options", "ENGINE=InnoDB").AutoMigrate(
		&model.KnowledgeDocument{},
		&model.RuleCategory{},
		&model.RuleVersion{},
		&model.RuleArticle{},
		&model.KnowledgeChunk{},
	))

	var engines int
	vhMust(t, pool.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM information_schema.tables
WHERE table_schema = ? AND engine = 'InnoDB'
  AND table_name IN (
    'knowledge_documents', 'rule_categories', 'rule_versions',
    'rule_articles', 'knowledge_chunks'
  )`, name).Scan(&engines))
	if engines != 5 {
		t.Fatalf("expected 5 InnoDB tables, got %d", engines)
	}

	var isolation string
	vhMust(t, pool.QueryRowContext(ctx,
		"SELECT @@transaction_isolation",
	).Scan(&isolation))
	if isolation != "READ-COMMITTED" {
		t.Fatalf("session baseline must be READ-COMMITTED, got %s", isolation)
	}
	t.Logf("SESSION_DEFAULT_ISOLATION=%s", isolation)

	return &exportMySQLSuite{
		db: db, writer: writer, pool: pool, name: name, uuid: uuid,
		at: time.Date(2026, 11, 1, 6, 0, 0, 123000000, time.UTC),
	}
}

func vhCandidate(f exportMySQLFixture) retrieval.Candidate {
	var expires *time.Time
	if f.version.ExpiresAt != nil {
		value := f.version.ExpiresAt.UTC().Truncate(time.Millisecond)
		expires = &value
	}
	return retrieval.Candidate{
		PublicationRevision: f.document.PublicationRevision,
		Chunk: retrieval.RuleChunk{
			ChunkID:     f.chunk.ID,
			DocumentID:  f.document.ID,
			VersionID:   f.version.ID,
			ArticleID:   f.article.ID,
			CategoryID:  f.category.ID,
			Version:     f.version.Version,
			EffectiveAt: f.version.EffectiveAt.UTC().Truncate(time.Millisecond),
			ExpiresAt:   expires,
			Score:       0.8,
			Highlights:  map[string][]string{},
		},
	}
}

func vhCall(
	s *exportMySQLSuite,
	candidates []retrieval.Candidate,
) (map[uint64]retrieval.HydratedChunk, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewKnowledgeRepository(s.db).HydrateCurrent(ctx, candidates, s.at)
}

func vhReleased(t *testing.T, s *exportMySQLSuite) {
	t.Helper()
	// The reader pool has exactly one connection. A leaked transaction
	// prevents this probe from completing.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var one int
	vhMust(t, s.pool.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	if one != 1 || s.pool.Stats().InUse != 0 {
		t.Fatal("reader connection was not released")
	}
}

func vhFailure(
	t *testing.T,
	got map[uint64]retrieval.HydratedChunk,
	err, want error,
) {
	t.Helper()
	if got != nil || !errors.Is(err, want) {
		t.Fatalf("expected nil result and %v; got result=%v err=%v",
			want, got, err)
	}
}

func vhUpdate(
	t *testing.T, s *exportMySQLSuite,
	table string, id uint64, field string, value any,
) {
	t.Helper()
	result := s.writer.Table(table).Where("id = ?", id).Update(field, value)
	vhMust(t, result.Error)
	if result.RowsAffected != 1 {
		t.Fatalf("fixture update affected %d rows", result.RowsAffected)
	}
}

// The hook runs immediately before the real detail SELECT.
// It neither replaces returned rows nor mocks the transaction.
func vhDetailHook(
	t *testing.T,
	s *exportMySQLSuite,
	hook func(*gorm.DB) error,
) *int {
	t.Helper()
	count := new(int)
	name := "test:vector_hydration_detail"
	vhMust(t, s.db.Callback().Row().Before("gorm:row").
		Register(name, func(tx *gorm.DB) {
			if !strings.Contains(tx.Statement.SQL.String(), "AS article_content") {
				return
			}
			*count = *count + 1
			if err := hook(tx); err != nil {
				tx.AddError(err)
			}
		}))
	t.Cleanup(func() {
		vhMust(t, s.db.Callback().Row().Remove(name))
	})
	return count
}

func vhTextBytes(f exportMySQLFixture) int {
	return len(f.version.Version) + len(f.version.Title) +
		len(f.version.SourceURL) + len(f.article.SectionPath) +
		len(f.article.ArticleNumber) + len(f.chunk.Content) +
		len(f.article.Content)
}

func vhSizedArticle(
	t *testing.T, s *exportMySQLSuite, f exportMySQLFixture, total int,
) string {
	t.Helper()
	n := total - (vhTextBytes(f) - len(f.article.Content))
	if n <= 0 {
		t.Fatal("invalid byte-budget fixture")
	}
	// Include multibyte Chinese text: the limit is bytes, not characters.
	content := strings.Repeat("中", n/3) + strings.Repeat("x", n%3)
	vhUpdate(t, s, "rule_articles", f.article.ID, "content", content)
	return content
}

func TestVectorHydrationMySQL(t *testing.T) {
	s := vhSuite(t)

	t.Run("authoritative_fields_and_two_bodies", func(t *testing.T) {
		f := s.fixture(t)
		vhUpdate(t, s, "knowledge_chunks", f.chunk.ID, "chunk_index", 2)
		vhUpdate(t, s, "rule_articles", f.article.ID, "article_order", 3)
		f.chunk.ChunkIndex = 2
		f.article.ArticleOrder = 3

		expires := s.at.Add(time.Hour)
		vhUpdate(t, s, "rule_versions", f.version.ID, "expires_at", expires)
		f.version.ExpiresAt = &expires
		c := vhCandidate(f)

		// These untrusted candidate fields must not become response content.
		c.Chunk.Title = "不可信候选标题"
		c.Chunk.Content = "不可信候选正文"
		c.Chunk.SourceURL = "https://example.invalid/untrusted"
		c.Chunk.Highlights["content"] = []string{"不应透传"}

		got, err := vhCall(s, []retrieval.Candidate{c})
		vhMust(t, err)
		want := retrieval.HydratedChunk{
			Chunk: retrieval.RuleChunk{
				ChunkID: f.chunk.ID, DocumentID: f.document.ID,
				VersionID: f.version.ID, ArticleID: f.article.ID,
				CategoryID: f.category.ID,
				ChunkIndex: 2, ArticleOrder: 3,
				Version: f.version.Version, Title: f.version.Title,
				SectionPath:   f.article.SectionPath,
				ArticleNumber: f.article.ArticleNumber,
				Content:       f.chunk.Content, SourceURL: f.version.SourceURL,
				EffectiveAt: c.Chunk.EffectiveAt, ExpiresAt: &expires,
				Highlights: map[string][]string{},
			},
			ArticleContent: f.article.Content,
		}
		if len(got) != 1 || !reflect.DeepEqual(got[f.chunk.ID], want) {
			t.Fatalf("authoritative result differs: %+v", got)
		}
		if want.Chunk.Content == want.ArticleContent {
			t.Fatal("fixture does not distinguish fragment and article")
		}
		// Hydration does not assign the candidate similarity score.
		if got[f.chunk.ID].Chunk.Score != 0 {
			t.Fatal("hydrator unexpectedly assigned a similarity score")
		}
		*got[f.chunk.ID].Chunk.ExpiresAt = time.Time{}
		if !c.Chunk.ExpiresAt.Equal(expires) {
			t.Fatal("hydrated expiry aliases candidate storage")
		}
		vhReleased(t, s)
	})

	t.Run("current_state", func(t *testing.T) {
		for _, mode := range []string{
			"current_nil", "current_other", "wrong_owner",
			"draft", "withdrawn_status", "withdrawn_at",
			"future", "null_effective", "expires_equal", "expires_before",
			"revision_changed", "category_changed", "article_link_changed",
			"effective_changed", "expiry_added", "expiry_removed",
			"expiry_changed", "version_changed", "missing_chunk",
			"effective_equal", "expires_after",
		} {
			t.Run(mode, func(t *testing.T) {
				f := s.fixture(t)
				if mode == "expiry_removed" || mode == "expiry_changed" {
					expires := s.at.Add(time.Hour)
					vhUpdate(t, s, "rule_versions", f.version.ID,
						"expires_at", expires)
					f.version.ExpiresAt = &expires
				}
				c := vhCandidate(f)
				wantCount := 0

				switch mode {
				case "current_nil":
					vhUpdate(t, s, "knowledge_documents",
						f.document.ID, "current_version_id", nil)
				case "current_other":
					other := s.fixture(t)
					vhUpdate(t, s, "knowledge_documents",
						f.document.ID, "current_version_id", other.version.ID)
				case "wrong_owner":
					other := s.fixture(t)
					vhUpdate(t, s, "rule_versions",
						other.version.ID, "version", "other-v1")
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "document_id", other.document.ID)
				case "draft":
					vhUpdate(t, s, "rule_versions", f.version.ID, "status", "draft")
				case "withdrawn_status":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "status", "withdrawn")
				case "withdrawn_at":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "withdrawn_at", s.at)
				case "future":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "effective_at", s.at.Add(time.Millisecond))
				case "null_effective":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "effective_at", nil)
				case "expires_equal":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "expires_at", s.at)
				case "expires_before":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "expires_at", s.at.Add(-time.Millisecond))
				case "revision_changed":
					vhUpdate(t, s, "knowledge_documents",
						f.document.ID, "publication_revision", 2)
				case "category_changed":
					other := s.fixture(t)
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "category_id", other.category.ID)
				case "article_link_changed":
					other := s.fixture(t)
					vhUpdate(t, s, "knowledge_chunks",
						f.chunk.ID, "chunk_index", 2)
					vhUpdate(t, s, "knowledge_chunks",
						f.chunk.ID, "article_id", other.article.ID)
					// Avoid the other article's existing ChunkIndex=0.
					// This branch is handled separately below.
				case "effective_changed":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "effective_at", s.at.Add(-2*time.Hour))
				case "expiry_added", "expires_after":
					expires := s.at.Add(time.Millisecond)
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "expires_at", expires)
					if mode == "expires_after" {
						c.Chunk.ExpiresAt = &expires
						wantCount = 1
					}
				case "expiry_removed":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "expires_at", nil)
				case "expiry_changed":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "expires_at", s.at.Add(2*time.Hour))
				case "version_changed":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "version", "changed-v2")
				case "missing_chunk":
					c.Chunk.ChunkID = math.MaxUint64
				case "effective_equal":
					vhUpdate(t, s, "rule_versions",
						f.version.ID, "effective_at", s.at)
					c.Chunk.EffectiveAt = s.at
					wantCount = 1
				}

				got, err := vhCall(s, []retrieval.Candidate{c})
				vhMust(t, err)
				if got == nil || len(got) != wantCount {
					t.Fatalf("expected %d current rows, got %+v", wantCount, got)
				}
				vhReleased(t, s)
			})
		}
	})

	t.Run("candidate_metadata_mismatch", func(t *testing.T) {
		for _, field := range []string{
			"document", "version_id", "article", "category",
			"revision", "version", "effective",
		} {
			t.Run(field, func(t *testing.T) {
				f := s.fixture(t)
				c := vhCandidate(f)
				switch field {
				case "document":
					c.Chunk.DocumentID++
				case "version_id":
					c.Chunk.VersionID++
				case "article":
					c.Chunk.ArticleID++
				case "category":
					c.Chunk.CategoryID++
				case "revision":
					c.PublicationRevision++
				case "version":
					c.Chunk.Version = "stale"
				case "effective":
					c.Chunk.EffectiveAt = c.Chunk.EffectiveAt.Add(-time.Millisecond)
				}
				got, err := vhCall(s, []retrieval.Candidate{c})
				vhMust(t, err)
				if got == nil || len(got) != 0 {
					t.Fatal("mismatched metadata was accepted")
				}
				vhReleased(t, s)
			})
		}
	})

	t.Run("invalid_and_empty_inputs", func(t *testing.T) {
		f := s.fixture(t)
		for _, mode := range []string{
			"nil", "too_many", "duplicate", "zero_id", "zero_document",
			"zero_version", "zero_article", "zero_category", "blank_version",
			"invalid_utf8", "long_version", "nan", "infinity", "score_range",
			"future", "zero_effective", "expired", "zero_at", "no_deadline",
		} {
			t.Run(mode, func(t *testing.T) {
				c := vhCandidate(f)
				cs := []retrieval.Candidate{c}
				at := s.at
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				switch mode {
				case "nil":
					cs = nil
				case "too_many":
					cs = make([]retrieval.Candidate, retrieval.MaxCandidates+1)
				case "duplicate":
					cs = append(cs, c)
				case "zero_id":
					cs[0].Chunk.ChunkID = 0
				case "zero_document":
					cs[0].Chunk.DocumentID = 0
				case "zero_version":
					cs[0].Chunk.VersionID = 0
				case "zero_article":
					cs[0].Chunk.ArticleID = 0
				case "zero_category":
					cs[0].Chunk.CategoryID = 0
				case "blank_version":
					cs[0].Chunk.Version = " "
				case "invalid_utf8":
					cs[0].Chunk.Version = string([]byte{0xff})
				case "long_version":
					cs[0].Chunk.Version = strings.Repeat("x", 257)
				case "nan":
					cs[0].Chunk.Score = math.NaN()
				case "infinity":
					cs[0].Chunk.Score = math.Inf(1)
				case "score_range":
					cs[0].Chunk.Score = 1.01
				case "future":
					cs[0].Chunk.EffectiveAt = at.Add(time.Millisecond)
				case "zero_effective":
					cs[0].Chunk.EffectiveAt = time.Time{}
				case "expired":
					expired := at
					cs[0].Chunk.ExpiresAt = &expired
				case "zero_at":
					at = time.Time{}
				case "no_deadline":
					ctx = context.Background()
				}

				got, err := NewKnowledgeRepository(s.db).HydrateCurrent(ctx, cs, at)
				vhFailure(t, got, err, retrieval.ErrInvalidCandidates)
				vhReleased(t, s)
			})
		}

		t.Run("empty_without_connection", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			held, err := s.pool.Conn(ctx)
			vhMust(t, err)
			defer held.Close()

			got, err := NewKnowledgeRepository(s.db).HydrateCurrent(
				ctx, []retrieval.Candidate{}, s.at)
			vhMust(t, err)
			if got == nil || len(got) != 0 {
				t.Fatal("empty input must return a nonnil empty map")
			}
		})

		t.Run("nil_repository", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var repo *KnowledgeRepository
			got, err := repo.HydrateCurrent(
				ctx, []retrieval.Candidate{vhCandidate(f)}, s.at)
			vhFailure(t, got, err, retrieval.ErrHydrationFailed)
		})
	})

	t.Run("bad_body_has_no_partial_result", func(t *testing.T) {
		for _, mode := range []string{"fragment", "article", "article_order"} {
			t.Run(mode, func(t *testing.T) {
				good, bad := s.fixture(t), s.fixture(t)
				switch mode {
				case "fragment":
					vhUpdate(t, s, "knowledge_chunks", bad.chunk.ID, "content", " \t")
				case "article":
					vhUpdate(t, s, "rule_articles", bad.article.ID, "content", " \n")
				case "article_order":
					vhUpdate(t, s, "rule_articles", bad.article.ID, "article_order", 0)
				}
				got, err := vhCall(s, []retrieval.Candidate{
					vhCandidate(good), vhCandidate(bad),
				})
				vhFailure(t, got, err, retrieval.ErrHydrationFailed)
				vhReleased(t, s)
			})
		}
	})

	t.Run("byte_budgets", func(t *testing.T) {
		for _, mode := range []string{
			"row_exact", "row_plus_one", "batch_exact", "batch_plus_one",
		} {
			t.Run(mode, func(t *testing.T) {
				count, total := 1, retrieval.MaxHydrationRowBytes
				wantLimit := false
				switch mode {
				case "row_plus_one":
					total++
					wantLimit = true
				case "batch_exact":
					count, total = 8, retrieval.MaxHydrationTotalBytes
				case "batch_plus_one":
					count, total = 9, retrieval.MaxHydrationTotalBytes+1
					wantLimit = true
				}

				cs := make([]retrieval.Candidate, 0, count)
				expected := make(map[uint64]string)
				for i := 0; i < count; i++ {
					f := s.fixture(t)
					size := total / count
					if i < total%count {
						size++
					}
					expected[f.chunk.ID] = vhSizedArticle(t, s, f, size)
					cs = append(cs, vhCandidate(f))
				}
				got, err := vhCall(s, cs)
				if wantLimit {
					vhFailure(t, got, err, retrieval.ErrHydrationLimit)
					if !errors.Is(err, retrieval.ErrHydrationFailed) {
						t.Fatal("limit error lost hydration failure classification")
					}
				} else {
					vhMust(t, err)
					if len(got) != count {
						t.Fatal("exact budget boundary was rejected")
					}
					for id, body := range expected {
						if got[id].ArticleContent != body {
							t.Fatal("body was truncated or changed")
						}
					}
				}
				vhReleased(t, s)
			})
		}
	})

	t.Run("readonly_transaction", func(t *testing.T) {
		f := s.fixture(t)
		calls := vhDetailHook(t, s, func(tx *gorm.DB) error {
			// An UPDATE matching no row still must be rejected as a write.
			_, err := tx.Statement.ConnPool.ExecContext(
				tx.Statement.Context,
				"UPDATE knowledge_documents SET publication_revision = publication_revision WHERE id = 0",
			)
			var mysqlErr *driver.MySQLError
			if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1792 {
				return fmt.Errorf("expected MySQL read-only error 1792, got %v", err)
			}
			return nil
		})
		got, err := vhCall(s, []retrieval.Candidate{vhCandidate(f)})
		vhMust(t, err)
		if len(got) != 1 || *calls != 1 {
			t.Fatal("read-only probe did not run inside detail transaction")
		}
		vhReleased(t, s)
	})

	t.Run("repeatable_read_publication", func(t *testing.T) {
		f := s.fixture(t)
		c := vhCandidate(f)
		calls := vhDetailHook(t, s, func(tx *gorm.DB) error {
			return s.writer.Model(&model.KnowledgeDocument{}).
				Where("id = ?", f.document.ID).
				Updates(map[string]any{
					"current_version_id":   nil,
					"publication_revision": 2,
				}).Error
		})

		got, err := vhCall(s, []retrieval.Candidate{c})
		vhMust(t, err)
		if *calls != 1 || len(got) != 1 ||
			got[f.chunk.ID].ArticleContent != f.article.Content {
			t.Fatal("publication change broke the existing read snapshot")
		}
		var current *uint64
		var revision uint64
		vhMust(t, s.writer.Raw(
			"SELECT current_version_id, publication_revision FROM knowledge_documents WHERE id = ?",
			f.document.ID,
		).Row().Scan(&current, &revision))
		if current != nil || revision != 2 {
			t.Fatal("independent publication mutation was not committed")
		}

		got, err = vhCall(s, []retrieval.Candidate{c})
		vhMust(t, err)
		if got == nil || len(got) != 0 || *calls != 1 {
			t.Fatal("new transaction did not observe committed publication state")
		}
		vhReleased(t, s)
	})

	t.Run("repeatable_read_budget_and_body", func(t *testing.T) {
		f := s.fixture(t)
		c := vhCandidate(f)
		oversized := strings.Repeat("中", retrieval.MaxHydrationRowBytes/3+1)
		calls := vhDetailHook(t, s, func(tx *gorm.DB) error {
			return s.writer.Model(&model.RuleArticle{}).
				Where("id = ?", f.article.ID).Update("content", oversized).Error
		})

		got, err := vhCall(s, []retrieval.Candidate{c})
		vhMust(t, err)
		if *calls != 1 || len(got) != 1 ||
			got[f.chunk.ID].ArticleContent != f.article.Content {
			t.Fatal("detail body did not share the budget snapshot")
		}
		var stored string
		vhMust(t, s.writer.Raw(
			"SELECT content FROM rule_articles WHERE id = ?", f.article.ID,
		).Row().Scan(&stored))
		if stored != oversized {
			t.Fatal("independent oversized-body update was not committed")
		}

		got, err = vhCall(s, []retrieval.Candidate{c})
		vhFailure(t, got, err, retrieval.ErrHydrationLimit)
		if *calls != 1 {
			t.Fatal("over-budget request reached detail SELECT")
		}
		vhReleased(t, s)
	})

	t.Run("cancellation_and_connection_failure", func(t *testing.T) {
		t.Run("cancelled_before_call", func(t *testing.T) {
			f := s.fixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			cancel()
			got, err := NewKnowledgeRepository(s.db).HydrateCurrent(
				ctx, []retrieval.Candidate{vhCandidate(f)}, s.at)
			vhFailure(t, got, err, context.Canceled)
			vhReleased(t, s)
		})

		t.Run("deadline_waiting_for_pool", func(t *testing.T) {
			f := s.fixture(t)
			acquireCtx, releaseAcquire := context.WithTimeout(
				context.Background(), time.Second)
			defer releaseAcquire()
			held, err := s.pool.Conn(acquireCtx)
			vhMust(t, err)
			defer held.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			got, err := NewKnowledgeRepository(s.db).HydrateCurrent(
				ctx, []retrieval.Candidate{vhCandidate(f)}, s.at)
			vhFailure(t, got, err, context.DeadlineExceeded)
			vhMust(t, held.Close())
			vhReleased(t, s)
		})

		t.Run("cancel_between_selects", func(t *testing.T) {
			f := s.fixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := vhDetailHook(t, s, func(tx *gorm.DB) error {
				cancel()
				return nil
			})
			got, err := NewKnowledgeRepository(s.db).HydrateCurrent(
				ctx, []retrieval.Candidate{vhCandidate(f)}, s.at)
			vhFailure(t, got, err, context.Canceled)
			if *calls != 1 {
				t.Fatal("cancellation hook did not run")
			}
			vhReleased(t, s)
		})

		t.Run("real_connection_lost_between_selects", func(t *testing.T) {
			f := s.fixture(t)
			calls := vhDetailHook(t, s, func(tx *gorm.DB) error {
				var id uint64
				if err := tx.Statement.ConnPool.QueryRowContext(
					tx.Statement.Context, "SELECT CONNECTION_ID()",
				).Scan(&id); err != nil {
					return err
				}
				if id == 0 {
					return errors.New("invalid reader connection ID")
				}
				// Kill only this test transaction's own reader connection.
				return s.writer.Exec(
					fmt.Sprintf("KILL CONNECTION %d", id),
				).Error
			})
			got, err := vhCall(s, []retrieval.Candidate{vhCandidate(f)})
			vhFailure(t, got, err, retrieval.ErrHydrationFailed)
			if *calls != 1 {
				t.Fatal("connection failure was not injected between SELECTs")
			}
			vhReleased(t, s)
		})
	})

	vhReleased(t, s)
}
