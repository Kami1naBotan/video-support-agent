//go:build workerdual

package workeracceptance

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/eventbus"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
)

type lostESVerification struct{ *elasticsearch.Client }

func (e lostESVerification) VerifyIndex(ctx context.Context, name string, docs []indexing.EncodedDocument) error {
	if err := e.Client.VerifyIndex(ctx, name, docs); err != nil {
		return err
	}
	return elasticsearch.ErrUnavailable
}

type lostMilvusBuild struct{ *milvus.Client }

func (v lostMilvusBuild) BuildCollection(ctx context.Context, name string, p *milvus.PreparedImport) (*milvus.BuildResult, error) {
	r, e := v.Client.BuildCollection(ctx, name, p)
	if e != nil {
		return r, e
	}
	return r, milvus.ErrStateUnknown
}

type partialESBuild struct{ *elasticsearch.Client }

func (e partialESBuild) BulkIndex(ctx context.Context, name string, docs []indexing.EncodedDocument) error {
	if len(docs) < 2 {
		return errors.New("partial fixture needs two documents")
	}
	if err := e.Client.BulkIndex(ctx, name, docs[:1]); err != nil {
		return err
	}
	return elasticsearch.ErrUnavailable
}

type partialMilvusBuild struct{ *milvus.Client }

func (v partialMilvusBuild) BuildCollection(ctx context.Context, name string, p *milvus.PreparedImport) (*milvus.BuildResult, error) {
	if len(p.Records) < 2 {
		return nil, errors.New("partial fixture needs two records")
	}
	subset := *p
	subset.Records = p.Records[:1]
	r, err := v.Client.BuildCollection(ctx, name, &subset)
	if err != nil {
		return r, err
	}
	if _, err = v.Client.VerifyCollection(ctx, r, &subset); err != nil {
		return nil, err
	}
	return r, milvus.ErrStateUnknown
}
func TestWorkerDualPartialRecovery(t *testing.T) {
	for _, side := range []string{"es_ack", "milvus_ack", "es_partial", "milvus_partial"} {
		t.Run(side, func(t *testing.T) {
			f := newDual(t)
			source := f.source(t)
			version, err := strconv.ParseUint(source.VersionID, 10, 64)
			must(t, err)
			must(t, f.db.Omit("Version").Create(&model.RuleArticle{VersionID: version, ArticleOrder: 2, SectionPath: "测试", ArticleNumber: "2", Content: "Synthetic second rule for partial writes."}).Error)
			embedder := controlledEmbedding{}
			ctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
			defer c()
			var es knowledgeworker.ESBuildWriter = f.es
			var vector knowledgeworker.MilvusBuildWriter = f.vector
			if side == "es_ack" {
				es = lostESVerification{f.es}
			} else {
				vector = lostMilvusBuild{f.vector}
			}
			if side == "es_partial" {
				es = partialESBuild{f.es}
			}
			if side == "milvus_partial" {
				vector = partialMilvusBuild{f.vector}
			}
			b, e := knowledgeworker.NewDualBuilder(f.repo, embedder, es, vector, f.options)
			must(t, e)
			claim, e := f.repo.ClaimLatest(ctx)
			must(t, e)
			if claim == nil {
				t.Fatal("no claim")
			}
			result, e := b.Build(ctx, *claim)
			if !errors.Is(e, knowledgeworker.ErrDualExternal) || result != nil {
				t.Fatalf("partial failure=%v", e)
			}
			snapshot, e := f.repo.PrepareSnapshot(ctx, *claim)
			must(t, e)
			if len(snapshot.Prepared.Documents) != 2 {
				t.Fatal("partial fixture requires exactly two chunks")
			}
			if side == "es_partial" {
				must(t, f.es.VerifyIndex(ctx, claim.ESIndexName, snapshot.Prepared.Documents[:1]))
				if err := f.es.VerifyIndex(ctx, claim.ESIndexName, snapshot.Prepared.Documents); err == nil {
					t.Fatal("incomplete ES accepted as complete")
				}
			} else {
				must(t, f.es.VerifyIndex(ctx, claim.ESIndexName, snapshot.Prepared.Documents))
			}
			var chunk model.KnowledgeChunk
			must(t, f.db.Where("article_id IN (?)", f.db.Model(&model.RuleArticle{}).Select("id").Where("version_id = ?", source.VersionID)).Take(&chunk).Error)
			var control model.KnowledgeIndexControl
			must(t, f.db.Take(&control).Error)
			if control.ActiveGenerationID != nil {
				t.Fatal("partial generation became active")
			}
			must(t, f.repo.RecordBuildFailure(ctx, *claim, knowledgefailure.CodeDualExternalFailed))
			_, e = f.repo.RecoverPending(ctx)
			must(t, e)
			var row model.EventOutbox
			must(t, f.db.Where("topic = ?", eventbus.TopicIndexRetry).Take(&row).Error)
			must(t, f.repo.AcceptRetry(ctx, eventbus.Delivery{Topic: row.Topic, Key: []byte(row.MessageKey), Value: row.Payload, Headers: []eventbus.Header{{Key: "message_id", Value: []byte(row.MessageID)}, {Key: "trace_id", Value: []byte(row.TraceID)}}}))
			var attempt model.KnowledgeIndexAttempt
			must(t, f.db.Where("attempt = 2").Take(&attempt).Error)
			early, e := f.repo.ClaimLatest(ctx)
			must(t, e)
			if early != nil {
				t.Fatal("bypassed recovery due time")
			}
			if wait := time.Until(attempt.NotBefore) + 50*time.Millisecond; wait > 0 {
				time.Sleep(wait)
			}
			// A fresh repository and builder represent restart with only durable state retained.
			restarted, e := repository.NewKnowledgeBuildRepository(f.db, 100)
			must(t, e)
			f.repo = restarted
			next, e := f.repo.ClaimLatest(ctx)
			must(t, e)
			if next == nil || next.Attempt != 2 || next.GenerationID == claim.GenerationID || next.ESIndexName == claim.ESIndexName || next.MilvusCollection == claim.MilvusCollection {
				t.Fatal("recovery reused partial targets")
			}
			b, e = knowledgeworker.NewDualBuilder(f.repo, embedder, f.es, f.vector, f.options)
			must(t, e)
			verified, e := b.Build(ctx, *next)
			must(t, e)
			must(t, f.repo.Publish(ctx, verified))
			resolver, e := repository.NewKnowledgeActiveRepository(f.db)
			must(t, e)
			active, e := resolver.ResolveActive(ctx)
			must(t, e)
			if active.GenerationID != next.GenerationID || active.ESIndex != next.ESIndexName || active.MilvusCollection != next.MilvusCollection || active.ManifestSHA256 != verified.Snapshot.ManifestSHA256 {
				t.Fatal("wrong active generation")
			}
			var retained model.KnowledgeChunk
			must(t, f.db.First(&retained, chunk.ID).Error)
			if retained.ID != chunk.ID || retained.Content != chunk.Content {
				t.Fatal("chunk identity/content changed")
			}
			workerSQLCount(t, f.db, &model.EventOutbox{}, 2)
			observed, e := f.repo.Observe(ctx)
			must(t, e)
			if observed.ActiveEpoch != 1 || observed.DesiredEpoch != 1 || len(observed.Attempts) != 2 {
				t.Fatal("observation mismatch")
			}
			for _, a := range observed.Attempts {
				if a.EventID != source.EventID || a.TraceID != source.TraceID || a.GenerationID == nil {
					t.Fatal("missing correlation")
				}
			}
			if e := f.repo.Publish(ctx, &knowledgeworker.VerifiedGeneration{Claim: *claim, Snapshot: snapshot, Identity: verified.Identity}); e == nil {
				t.Fatal("abandoned generation published")
			}
			t.Logf("side=%s old=%s recovered=%s stable_chunk=%d event=%s trace=%s attempts=%d", side, claim.GenerationID, next.GenerationID, chunk.ID, source.EventID, source.TraceID, len(observed.Attempts))
		})
	}
}
