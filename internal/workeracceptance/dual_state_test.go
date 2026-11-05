//go:build workerdual

package workeracceptance

import (
	"context"
	"errors"
	"testing"
	"time"
	"video-support-agent/internal/knowledgeevent"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
)

func TestWorkerDualEmptyAndExpiry(t *testing.T) {
	for _, mode := range []string{"expired", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			f := newDual(t)
			s := f.source(t)
			ctx := context.Background()
			embedder := controlledEmbedding{}
			expires := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Millisecond)
			if mode == "expired" {
				must(t, f.db.Model(&model.RuleVersion{}).Where("id = ?", s.VersionID).Update("expires_at", expires).Error)
			}
			verified := f.build(t, embedder)
			f.verifyPublished(t, verified, embedder)
			if mode == "expired" {
				at := expires.Add(time.Millisecond)
				hits, e := f.es.SearchCandidatesAt(ctx, verified.Claim.ESIndexName, "Synthetic", retrieval.SearchOptions{Limit: 10}, at)
				must(t, e)
				searcher, e := milvus.NewSearcher(embedder, f.vector, 10*time.Second)
				must(t, e)
				vhits, e := searcher.SearchCandidatesAt(ctx, verified.Claim.MilvusCollection, verified.Identity, "Synthetic", retrieval.SearchOptions{Limit: 10}, at)
				must(t, e)
				if len(hits) != 0 || len(vhits) != 0 {
					t.Fatal("expired target returned content")
				}
				must(t, f.db.Model(&model.RuleVersion{}).Where("id = ?", s.VersionID).Update("expires_at", time.Now().UTC().Add(-time.Second)).Error)
			} else {
				now := time.Now().UTC()
				must(t, f.db.Model(&model.RuleVersion{}).Where("id = ?", s.VersionID).Updates(map[string]any{"status": model.RuleVersionStatusWithdrawn, "withdrawn_at": now}).Error)
				must(t, f.db.Model(&model.KnowledgeDocument{}).Where("id = ?", s.DocumentID).Updates(map[string]any{"current_version_id": nil, "publication_revision": 2}).Error)
				s.EventType = knowledgeevent.VersionWithdrawn
				s.PublicationRevision = "2"
			}
			must(t, f.db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 2).Error)
			s.EventID, _ = knowledgeevent.NewID()
			inbox, e := repository.NewKnowledgeInboxRepository(f.db)
			must(t, e)
			must(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, s)))
			empty := f.build(t, embedder)
			if len(empty.Snapshot.Prepared.Documents) != 0 {
				t.Fatal("snapshot not empty")
			}
			must(t, f.repo.Publish(ctx, empty))
			activeRepo, e := repository.NewKnowledgeActiveRepository(f.db)
			must(t, e)
			active, e := activeRepo.ResolveActive(ctx)
			must(t, e)
			if !active.Empty || active.ESIndex != "" || active.MilvusCollection != "" || active.Epoch != 2 {
				t.Fatal("empty active pair invalid")
			}
			if e = f.repo.Publish(ctx, verified); e != nil && !errors.Is(e, knowledgeworker.ErrBuildObsolete) && !errors.Is(e, knowledgeworker.ErrBuildLeaseLost) {
				t.Fatal(e)
			}
			after, e := activeRepo.ResolveActive(ctx)
			must(t, e)
			if after.GenerationID != active.GenerationID {
				t.Fatal("old generation overwrote empty")
			}
			t.Logf("%s: query validity and empty active epoch=2 verified; old physical targets retained but not selected", mode)
		})
	}
}
func TestWorkerDualCapacityAndObsolete(t *testing.T) {
	f := newDual(t)
	s := f.source(t)
	ctx := context.Background()
	e := controlledEmbedding{}
	first := f.build(t, e)
	f.verifyPublished(t, first, e)
	must(t, f.db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Updates(map[string]any{"desired_epoch": 2, "generation_limit": 1}).Error)
	s.EventID, _ = knowledgeevent.NewID()
	inbox, err := repository.NewKnowledgeInboxRepository(f.db)
	must(t, err)
	must(t, inbox.AcceptSource(ctx, workerSQLDelivery(t, s)))
	limited, err := repository.NewKnowledgeBuildRepository(f.db, 1)
	must(t, err)
	if claim, err := limited.ClaimLatest(ctx); !errors.Is(err, knowledgeworker.ErrBuildCapacity) || claim != nil {
		t.Fatalf("capacity=%v", err)
	}
	workerSQLCount(t, f.db, &model.KnowledgeIndexGeneration{}, 1)
	must(t, f.db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("generation_limit", 100).Error)
	later := f.build(t, e)
	must(t, f.db.Model(&model.KnowledgeIndexControl{}).Where("id = 1").Update("desired_epoch", 3).Error)
	if err = f.repo.Publish(ctx, later); !errors.Is(err, knowledgeworker.ErrBuildObsolete) {
		t.Fatalf("obsolete=%v", err)
	}
	resolver, err := repository.NewKnowledgeActiveRepository(f.db)
	must(t, err)
	active, err := resolver.ResolveActive(ctx)
	must(t, err)
	if active.GenerationID != first.Claim.GenerationID {
		t.Fatal("unpublished/obsolete pair selected")
	}
	t.Log("capacity held at one reserved pair; after authorized test limit increase, epoch2 built real targets but epoch3 prevented publication")
}
