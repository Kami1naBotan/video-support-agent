//go:build searchintegration

package searchintegration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestSearchMySQLCurrent(t *testing.T) {
	s := newSearchSuite(t)

	runCase(t, "empty_without_sql", func(t *testing.T) {
		count := s.rowHook(t, nil)
		s.verify(t, nil)
		if *count != 0 {
			t.Fatal("空候选执行了状态查询")
		}
	})

	runCase(t, "250_candidates_one_parameterized_query", func(t *testing.T) {
		first := s.fixture(t)
		candidates := []retrieval.Candidate{first}
		ids := []uint64{first.Chunk.ChunkID}
		for i := 1; i < 250; i++ {
			chunk := model.KnowledgeChunk{
				ArticleID:  first.Chunk.ArticleID,
				ChunkIndex: uint32(i), Content: "批量测试片段",
			}
			must(t, s.db.Omit(clause.Associations).Create(&chunk).Error)
			candidate := first
			candidate.Chunk.ChunkID = chunk.ID
			candidate.Chunk.ChunkIndex = chunk.ChunkIndex
			candidates = append(candidates, candidate)
			ids = append(ids, chunk.ID)
		}
		count := s.rowHook(t, func(tx *gorm.DB) error {
			query := tx.Statement.SQL.String()
			if strings.Count(query, "?") != 253 || len(tx.Statement.Vars) != 253 {
				return fmt.Errorf("预期250个ID及状态/两个时刻参数，vars=%d", len(tx.Statement.Vars))
			}
			for i, id := range ids {
				if tx.Statement.Vars[i] != id {
					return fmt.Errorf("候选ID没有作为参数传入，位置=%d", i)
				}
			}
			if strings.Contains(strings.ToLower(query), ".content") {
				return errors.New("状态复核不应读取大正文")
			}
			return nil
		})
		s.verify(t, candidates, ids...)
		if *count != 1 {
			t.Fatalf("预期一次批量查询，实际 %d", *count)
		}
		t.Log("BATCH_QUERY_COUNT=1 CANDIDATES=250 PARAMETERIZED=true")
	})

	for _, tc := range []struct {
		name   string
		change func(*retrieval.Candidate)
	}{
		{"document", func(c *retrieval.Candidate) { c.Chunk.DocumentID++ }},
		{"version", func(c *retrieval.Candidate) { c.Chunk.VersionID++ }},
		{"article", func(c *retrieval.Candidate) { c.Chunk.ArticleID++ }},
		{"category", func(c *retrieval.Candidate) { c.Chunk.CategoryID++ }},
		{"revision", func(c *retrieval.Candidate) { c.PublicationRevision++ }},
		{"chunk_index", func(c *retrieval.Candidate) { c.Chunk.ChunkIndex++ }},
		{"article_order", func(c *retrieval.Candidate) { c.Chunk.ArticleOrder++ }},
		{"effective_time", func(c *retrieval.Candidate) {
			c.Chunk.EffectiveAt = c.Chunk.EffectiveAt.Add(time.Millisecond)
		}},
		{"expiry_presence", func(c *retrieval.Candidate) {
			expiry := s.at.Add(time.Hour)
			c.Chunk.ExpiresAt = &expiry
		}},
	} {
		runCase(t, "stale_candidate_"+tc.name, func(t *testing.T) {
			valid := s.fixture(t)
			stale := s.fixture(t)
			tc.change(&stale)
			s.verify(t, []retrieval.Candidate{stale, valid}, valid.Chunk.ChunkID)
		})
	}

	for _, tc := range []struct {
		name     string
		column   string
		value    any
		accepted bool
	}{
		{"draft", "status", "draft", false},
		{"withdrawn_status", "status", "withdrawn", false},
		{"withdrawn_time", "withdrawn_at", s.at, false},
		{"effective_null", "effective_at", nil, false},
		{"effective_future", "effective_at", s.at.Add(time.Millisecond), false},
		{"effective_equal", "effective_at", s.at, true},
		{"expiry_equal", "expires_at", s.at, false},
		{"expiry_past", "expires_at", s.at.Add(-time.Millisecond), false},
		{"expiry_future", "expires_at", s.at.Add(time.Millisecond), true},
	} {
		runCase(t, tc.name, func(t *testing.T) {
			c := s.fixture(t)
			must(t, s.db.Model(&model.RuleVersion{}).
				Where("id = ?", c.Chunk.VersionID).Update(tc.column, tc.value).Error)
			if value, ok := tc.value.(time.Time); ok {
				switch tc.column {
				case "effective_at":
					c.Chunk.EffectiveAt = value
				case "expires_at":
					c.Chunk.ExpiresAt = &value
				}
			}
			if tc.accepted {
				s.verify(t, []retrieval.Candidate{c}, c.Chunk.ChunkID)
			} else {
				s.verify(t, []retrieval.Candidate{c})
			}
		})
	}

	runCase(t, "different_nonnull_expiry", func(t *testing.T) {
		c := s.fixture(t)
		actual := s.at.Add(time.Hour)
		stale := actual.Add(time.Millisecond)
		must(t, s.db.Model(&model.RuleVersion{}).
			Where("id = ?", c.Chunk.VersionID).Update("expires_at", actual).Error)
		c.Chunk.ExpiresAt = &stale
		s.verify(t, []retrieval.Candidate{c})
		c.Chunk.ExpiresAt = &actual
		s.verify(t, []retrieval.Candidate{c}, c.Chunk.ChunkID)
	})

	runCase(t, "current_pointer_and_ownership", func(t *testing.T) {
		first, other := s.fixture(t), s.fixture(t)
		must(t, s.db.Model(&model.KnowledgeDocument{}).
			Where("id = ?", first.Chunk.DocumentID).
			Update("current_version_id", other.Chunk.VersionID).Error)
		s.verify(t, []retrieval.Candidate{first, other}, other.Chunk.ChunkID)
		must(t, s.db.Model(&model.KnowledgeDocument{}).
			Where("id = ?", other.Chunk.DocumentID).
			Update("current_version_id", nil).Error)
		s.verify(t, []retrieval.Candidate{first, other})
	})

	runCase(t, "moved_chunk_relationship", func(t *testing.T) {
		first, other := s.fixture(t), s.fixture(t)
		must(t, s.db.Model(&model.KnowledgeChunk{}).
			Where("id = ?", first.Chunk.ChunkID).
			Updates(map[string]any{
				"article_id": other.Chunk.ArticleID, "chunk_index": 1,
			}).Error)
		s.verify(t, []retrieval.Candidate{first, other}, other.Chunk.ChunkID)
	})

	for _, table := range []string{
		"knowledge_chunks", "rule_articles", "rule_versions",
		"knowledge_documents", "rule_categories",
	} {
		runCase(t, "missing_"+table, func(t *testing.T) {
			c := s.fixture(t)
			ids := map[string]uint64{
				"knowledge_chunks":    c.Chunk.ChunkID,
				"rule_articles":       c.Chunk.ArticleID,
				"rule_versions":       c.Chunk.VersionID,
				"knowledge_documents": c.Chunk.DocumentID,
				"rule_categories":     c.Chunk.CategoryID,
			}
			if table == "knowledge_chunks" {
				must(t, s.db.Delete(&model.KnowledgeChunk{}, ids[table]).Error)
			} else {
				s.removeParent(t, table, ids[table])
			}
			s.verify(t, []retrieval.Candidate{c})
		})
	}

	runCase(t, "invalid_candidates_before_sql", func(t *testing.T) {
		c := s.fixture(t)
		zero := c
		zero.Chunk.ChunkID = 0
		cases := [][]retrieval.Candidate{
			{zero}, {c, c}, make([]retrieval.Candidate, 251),
		}
		count := s.rowHook(t, nil)
		repo := repository.NewKnowledgeRepository(s.db)
		for _, candidates := range cases {
			got, err := repo.VerifyCurrent(context.Background(), candidates, s.at)
			if got != nil || !errors.Is(err, retrieval.ErrInvalidCandidates) {
				t.Fatalf("预期拒绝非法候选：got=%v err=%v", got, err)
			}
		}
		got, err := repo.VerifyCurrent(context.Background(), []retrieval.Candidate{c}, time.Time{})
		if got != nil || !errors.Is(err, retrieval.ErrInvalidCandidates) || *count != 0 {
			t.Fatalf("非法输入不应查询数据库：got=%v err=%v queries=%d", got, err, *count)
		}
	})

	runCase(t, "readonly_repeatable_read_snapshot", func(t *testing.T) {
		c := s.fixture(t)
		armed := true
		count := s.rowHook(t, func(tx *gorm.DB) error {
			if !armed {
				return nil
			}
			armed = false
			ctx := tx.Statement.Context
			pool := tx.Statement.ConnPool
			var revision uint64

			// 使用被测事务的真实连接建立快照；不替换被测SQL。
			if err := pool.QueryRowContext(ctx,
				"SELECT publication_revision FROM knowledge_documents WHERE id = ?",
				c.Chunk.DocumentID).Scan(&revision); err != nil {
				return err
			}
			if revision != 1 {
				return fmt.Errorf("意外初始修订号 %d", revision)
			}

			_, err := pool.ExecContext(ctx,
				"UPDATE knowledge_documents SET publication_revision = 99 WHERE id = ?",
				c.Chunk.DocumentID)
			var mysqlErr *driver.MySQLError
			if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1792 {
				return fmt.Errorf("预期真实只读事务拒绝写入1792，实际 %v", err)
			}
			// 第二连接提交更新，被测事务仍应读取旧快照。
			if err := s.writer.WithContext(ctx).Model(&model.KnowledgeDocument{}).
				Where("id = ?", c.Chunk.DocumentID).
				Update("publication_revision", 2).Error; err != nil {
				return err
			}
			t.Log("READ_ONLY_WRITE_REJECTED=1792 WRITER_COMMITTED_REVISION=2")
			return nil
		})
		s.verify(t, []retrieval.Candidate{c}, c.Chunk.ChunkID)
		s.verify(t, []retrieval.Candidate{c})
		c.PublicationRevision = 2
		s.verify(t, []retrieval.Candidate{c}, c.Chunk.ChunkID)
		if *count != 3 {
			t.Fatalf("预期三次独立查询，实际 %d", *count)
		}
		t.Log("RR_OLD_SNAPSHOT_ACCEPTED=true NEXT_SNAPSHOT_REJECTED_STALE=true")
	})

	runCase(t, "cancel_and_connection_recovery", func(t *testing.T) {
		c := s.fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		armed := true
		s.rowHook(t, func(*gorm.DB) error {
			if armed {
				armed = false
				cancel()
			}
			return nil
		})
		got, err := repository.NewKnowledgeRepository(s.db).
			VerifyCurrent(ctx, []retrieval.Candidate{c}, s.at)
		if got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("取消未传播：got=%v err=%v", got, err)
		}
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer pingCancel()
		must(t, s.pool.PingContext(pingCtx))
		s.verify(t, []retrieval.Candidate{c}, c.Chunk.ChunkID)
		t.Log("CANCEL_RECOVERY=true")
	})

	runCase(t, "sql_failure_no_partial_set", func(t *testing.T) {
		c := s.fixture(t)
		// 只重命名本次自建库中的表，随后立即恢复。
		must(t, s.db.Exec("RENAME TABLE knowledge_chunks TO search_hidden_chunks").Error)
		t.Cleanup(func() {
			must(t, s.db.Exec("RENAME TABLE search_hidden_chunks TO knowledge_chunks").Error)
		})
		got, err := repository.NewKnowledgeRepository(s.db).
			VerifyCurrent(context.Background(), []retrieval.Candidate{c}, s.at)
		if got != nil || !errors.Is(err, retrieval.ErrVerificationFailed) {
			t.Fatalf("SQL失败必须整体失败：got=%v err=%v", got, err)
		}
	})
}
