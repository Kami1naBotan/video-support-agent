package vectorrebuild

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/vectorization"
)

const rebuildSecret = "PRIVATE_RULE_CONTENT_92741"

var rebuildStages = []string{
	"alias-read", "export", "source-check", "vectorize",
	"prepare-import", "build", "verify", "publish", "complete",
}

var rebuildTrace = []string{
	"stage:alias-read", "alias",
	"stage:export", "export",
	"stage:source-check",
	"stage:vectorize", "embed",
	"stage:prepare-import",
	"stage:build", "build",
	"stage:verify", "verify",
	"stage:publish", "publish",
	"stage:complete",
}

type rebuildRig struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	source      *indexing.Snapshot
	options     Options
	identity    embedding.ModelIdentity
	trace       []string
	events      []json.RawMessage
	old         string
	failAt      string
	failure     error
	evidenceAt  string
	cancelAt    string
	nilBuild    bool
	nilToken    bool
	prepared    *milvus.PreparedImport
	build       *milvus.BuildResult
	token       *milvus.VerifiedTarget
	publication milvus.PublicationResult
}

func newRebuildRig(t *testing.T) *rebuildRig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	name := "rule_chunks_vec_v1_0123456789abcdef0123456789abcdef"
	r := &rebuildRig{
		t: t, ctx: ctx, cancel: cancel, old: "old_target",
		options: Options{
			NewCollection: name, ExpectedDatabase: "test_source",
			ExpectedServerUUID: "test-server",
			ExpectedDigest:     strings.Repeat("a", 64),
		},
		identity: embedding.ModelIdentity{
			Model: embedding.ModelName, Digest: strings.Repeat("a", 64),
			ServerVersion: "0.40.13", Dimensions: 1024,
			ContextLength: 8192, InputPolicy: embedding.InputPolicy,
		},
		failure: errors.New("isolated dependency failure"),
		// 仅作为接口替身传递的标记，不是有效的真实Milvus凭据。
		token: new(milvus.VerifiedTarget),
		publication: milvus.PublicationResult{
			State: milvus.PublicationApplied, ExpectedOld: "old_target",
			NewTarget: name, ObservedTarget: name,
			RequestAcknowledged: true, ObservationSucceeded: true,
		},
	}
	r.source = &indexing.Snapshot{
		SourceDatabase: "test_source", SourceServerUUID: "test-server",
		CapturedAt: at,
		Documents: []indexing.Document{{
			ChunkID: "1", DocumentID: "10", VersionID: "20",
			ArticleID: "30", CategoryID: "40", PublicationRevision: "2",
			ArticleOrder: 1, Version: "test-v1", Title: "虚构测试规则",
			Content: rebuildSecret, SourceURL: "https://example.invalid/test",
			Status: "published", IsCurrent: true,
			EffectiveAt: at.Add(-time.Hour),
		}},
	}
	return r
}

func (r *rebuildRig) step(ctx context.Context, name string) error {
	r.t.Helper()
	if ctx != r.ctx {
		r.t.Fatalf("%s received a different overall context", name)
	}
	r.trace = append(r.trace, name)
	if name == r.cancelAt {
		r.cancel()
	}
	if name == r.failAt {
		return r.failure
	}
	return nil
}

func (r *rebuildRig) ExportCurrent(ctx context.Context) (*indexing.Snapshot, error) {
	if err := r.step(ctx, "export"); err != nil {
		return nil, err
	}
	return r.source, nil
}

func (r *rebuildRig) EmbedWithIdentity(
	ctx context.Context, texts []string,
) (embedding.BatchResult, error) {
	if err := r.step(ctx, "embed"); err != nil {
		return embedding.BatchResult{}, err
	}
	if !reflect.DeepEqual(texts, []string{rebuildSecret}) {
		r.t.Fatal("wrong input text or extra embedding call")
	}
	vector := make([]float32, 1024)
	vector[0] = 1
	return embedding.BatchResult{
		Identity: r.identity, Vectors: [][]float32{vector},
	}, nil
}

func (r *rebuildRig) GetAliasTarget(ctx context.Context) (string, error) {
	if err := r.step(ctx, "alias"); err != nil {
		return "", err
	}
	return r.old, nil
}

func (r *rebuildRig) BuildCollection(
	ctx context.Context, name string, prepared *milvus.PreparedImport,
) (*milvus.BuildResult, error) {
	if name != r.options.NewCollection || prepared == nil ||
		prepared.SourceDatabase != r.options.ExpectedDatabase ||
		prepared.SourceServerUUID != r.options.ExpectedServerUUID ||
		prepared.Identity != r.identity || len(prepared.Records) != 1 {
		r.t.Fatal("wrong prepared import passed to writer")
	}
	record := prepared.Records[0]
	if record.ChunkID != "1" || record.DocumentID != "10" ||
		record.Category != "40" || record.Embedding[0] != 1 ||
		record.EffectiveAt != r.source.Documents[0].EffectiveAt.UnixMilli() {
		r.t.Fatal("record binding or time conversion changed")
	}
	r.prepared = prepared
	r.build = &milvus.BuildResult{
		Collection: name, CollectionID: 123,
		Stage: "ready-for-verification", Inserted: 1,
	}
	if err := r.step(ctx, "build"); err != nil {
		r.build.Stage = "insert"
		return r.build, err
	}
	if r.nilBuild {
		return nil, nil
	}
	return r.build, nil
}

func (r *rebuildRig) VerifyCollection(
	ctx context.Context, build *milvus.BuildResult, prepared *milvus.PreparedImport,
) (*milvus.VerifiedTarget, error) {
	if build != r.build || prepared != r.prepared {
		r.t.Fatal("verification received a different build/import")
	}
	if err := r.step(ctx, "verify"); err != nil {
		return nil, err
	}
	if r.nilToken {
		return nil, nil
	}
	return r.token, nil
}

func (r *rebuildRig) PublishVerified(
	ctx context.Context, old string, token *milvus.VerifiedTarget,
) (milvus.PublicationResult, error) {
	if old != r.old || token != r.token {
		r.t.Fatal("publication lost expected old target or verification token")
	}
	err := r.step(ctx, "publish")
	return r.publication, err
}

func (r *rebuildRig) report(result Result) error {
	r.trace = append(r.trace, "stage:"+result.Stage)
	raw, err := json.Marshal(result)
	if err != nil {
		r.t.Fatal(err)
	}
	if strings.Contains(string(raw), rebuildSecret) {
		r.t.Fatal("stage evidence contains full source content")
	}
	r.events = append(r.events, raw)
	if result.Stage == r.evidenceAt {
		return errors.New(rebuildSecret)
	}
	return nil
}

func (r *rebuildRig) run() (*Result, error) {
	return Rebuild(r.ctx, r, r, r, r.options, r.report)
}

func requireRebuildError(t *testing.T, result *Result, err, want error, stage string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want=%v", err, want)
	}
	if result == nil || result.Stage != stage {
		t.Fatalf("result=%+v, want stage=%s", result, stage)
	}
}

func TestVectorRebuildSuccess(t *testing.T) {
	r := newRebuildRig(t)
	got, err := r.run()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.trace, rebuildTrace) {
		t.Fatalf("unexpected sequence: %v", r.trace)
	}
	if got.Stage != "complete" || got.Count != 1 ||
		got.NewCollection != r.options.NewCollection ||
		got.OldCollection != "old_target" ||
		got.SourceDatabase != r.source.SourceDatabase ||
		got.SourceServerUUID != r.source.SourceServerUUID ||
		!got.CapturedAt.Equal(r.source.CapturedAt) ||
		got.Identity != r.identity ||
		got.Build != r.build || got.Verification == nil ||
		!reflect.DeepEqual(got.Publication, r.publication) {
		t.Fatalf("incomplete final result: %+v", got)
	}
	if len(r.events) != len(rebuildStages) {
		t.Fatal("missing stage evidence")
	}
}

func TestVectorRebuildPreflight(t *testing.T) {
	for _, mode := range []string{
		"no_deadline", "canceled", "source", "embedder", "writer", "reporter",
		"database", "uuid", "name", "digest",
	} {
		t.Run(mode, func(t *testing.T) {
			r := newRebuildRig(t)
			var source indexing.Source = r
			var embedder vectorization.BatchEmbedder = r
			var writer Writer = r
			reporter := r.report
			ctx := r.ctx
			var want error = milvus.ErrInput
			switch mode {
			case "no_deadline":
				ctx = context.Background()
			case "canceled":
				r.cancel()
				want = context.Canceled
			case "source":
				source = nil
			case "embedder":
				embedder = nil
			case "writer":
				writer = nil
			case "reporter":
				reporter = nil
			case "database":
				r.options.ExpectedDatabase = " test_source "
			case "uuid":
				r.options.ExpectedServerUUID = ""
			case "name":
				r.options.NewCollection = "current_alias"
			case "digest":
				r.options.ExpectedDigest = strings.Repeat("A", 64)
			}
			got, err := Rebuild(ctx, source, embedder, writer, r.options, reporter)
			requireRebuildError(t, got, err, want, "preflight")
			if len(r.trace) != 0 || got.Publication.State != milvus.PublicationNotAttempted {
				t.Fatal("preflight failure performed work")
			}
		})
	}
}

func TestVectorRebuildFailures(t *testing.T) {
	for _, tt := range []struct {
		at    string
		stage string
		want  error
	}{
		{"alias", "alias-read", milvus.ErrUnavailable},
		{"export", "export", indexing.ErrInvalidSource},
		{"embed", "vectorize", embedding.ErrUnavailable},
		{"build", "build", milvus.ErrStateUnknown},
		{"verify", "verify", milvus.ErrResponse},
		{"publish", "publish", milvus.ErrStateUnknown},
	} {
		t.Run(tt.at, func(t *testing.T) {
			r := newRebuildRig(t)
			r.failAt, r.failure = tt.at, tt.want
			got, err := r.run()
			requireRebuildError(t, got, err, tt.want, tt.stage)
			end := -1
			for i, step := range rebuildTrace {
				if step == tt.at {
					end = i
					break
				}
			}
			if !reflect.DeepEqual(r.trace, rebuildTrace[:end+1]) {
				t.Fatalf("failure retried or pipeline continued: %v", r.trace)
			}
			if tt.at != "publish" && got.Publication.State != milvus.PublicationNotAttempted {
				t.Fatal("earlier failure reported a publication")
			}
			if tt.at == "build" &&
				(got.Build == nil || got.Build.Stage != "insert") {
				t.Fatal("failed build evidence was lost")
			}
			if tt.at == "publish" && got.Publication.State != milvus.PublicationApplied {
				t.Fatal("publication observation was lost alongside error")
			}
		})
	}

	for _, mode := range []string{
		"same_target", "nil_snapshot", "wrong_database", "wrong_uuid",
		"empty_snapshot", "digest_mismatch", "nil_build", "nil_token",
	} {
		t.Run(mode, func(t *testing.T) {
			r := newRebuildRig(t)
			var want error = indexing.ErrInvalidSource
			stage := "source-check"
			switch mode {
			case "same_target":
				r.old = r.options.NewCollection
				want, stage = milvus.ErrInput, "alias-read"
			case "nil_snapshot":
				r.source = nil
			case "wrong_database":
				r.source.SourceDatabase = "another_db"
			case "wrong_uuid":
				r.source.SourceServerUUID = "another_server"
			case "empty_snapshot":
				r.source.Documents = []indexing.Document{}
				want, stage = embedding.ErrInput, "vectorize"
			case "digest_mismatch":
				r.identity.Digest = strings.Repeat("b", 64)
				want, stage = embedding.ErrModelMismatch, "prepare-import"
			case "nil_build":
				r.nilBuild = true
				want, stage = milvus.ErrResponse, "build"
			case "nil_token":
				r.nilToken = true
				want, stage = milvus.ErrResponse, "verify"
			}
			got, err := r.run()
			requireRebuildError(t, got, err, want, stage)
			for _, step := range r.trace {
				if step == "publish" {
					t.Fatal("invalid intermediate result reached publication")
				}
			}
		})
	}
}

func TestVectorRebuildEvidenceFailures(t *testing.T) {
	for _, stage := range rebuildStages {
		t.Run(stage, func(t *testing.T) {
			r := newRebuildRig(t)
			r.evidenceAt = stage
			got, err := r.run()
			requireRebuildError(t, got, err, ErrEvidence, stage)
			if strings.Contains(err.Error(), rebuildSecret) {
				t.Fatal("reporter error leaked")
			}
			stop := "stage:" + stage
			end := -1
			for i, step := range rebuildTrace {
				if step == stop {
					end = i
					break
				}
			}
			if !reflect.DeepEqual(r.trace, rebuildTrace[:end+1]) {
				t.Fatalf("evidence failure did not stop immediately: %v", r.trace)
			}
			if stage == "complete" {
				if got.Publication.State != milvus.PublicationApplied {
					t.Fatal("final evidence failure lost applied observation")
				}
			} else if got.Publication.State != milvus.PublicationNotAttempted {
				t.Fatal("publication occurred before evidence was accepted")
			}
		})
	}
}

func TestVectorRebuildPublicationStates(t *testing.T) {
	for _, state := range []milvus.PublicationState{
		milvus.PublicationNotAttempted,
		milvus.PublicationOldObserved,
		milvus.PublicationUnknown,
	} {
		t.Run(string(state), func(t *testing.T) {
			r := newRebuildRig(t)
			r.publication.State = state
			got, err := r.run()
			requireRebuildError(t, got, err, milvus.ErrStateUnknown, "publish")
			if !reflect.DeepEqual(got.Publication, r.publication) ||
				r.trace[len(r.trace)-1] != "publish" {
				t.Fatal("non-applied publication was reported complete")
			}
		})
	}
}

func TestVectorRebuildCancellation(t *testing.T) {
	for _, tt := range []struct{ at, stage string }{
		{"export", "export"},
		{"embed", "vectorize"},
		{"build", "build"},
		{"verify", "verify"},
		{"publish", "publish"},
	} {
		t.Run(tt.at, func(t *testing.T) {
			r := newRebuildRig(t)
			r.cancelAt = tt.at
			got, err := r.run()
			requireRebuildError(t, got, err, context.Canceled, tt.stage)
			if r.trace[len(r.trace)-1] != tt.at {
				t.Fatalf("canceled pipeline continued: %v", r.trace)
			}
			if tt.at == "publish" && got.Publication.State != milvus.PublicationApplied {
				t.Fatal("cancellation lost observed publication result")
			}
		})
	}
}
