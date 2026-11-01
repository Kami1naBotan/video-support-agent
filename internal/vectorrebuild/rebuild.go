package vectorrebuild

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/vectorization"
)

var ErrEvidence = errors.New("cannot write rebuild evidence")

type Options struct {
	NewCollection      string
	ExpectedDatabase   string
	ExpectedServerUUID string
	ExpectedDigest     string
}

type Result struct {
	Stage            string                     `json:"stage"`
	NewCollection    string                     `json:"new_collection"`
	OldCollection    string                     `json:"old_collection"`
	SourceDatabase   string                     `json:"source_database,omitempty"`
	SourceServerUUID string                     `json:"source_server_uuid,omitempty"`
	CapturedAt       time.Time                  `json:"captured_at"`
	Count            int                        `json:"count"`
	Identity         embedding.ModelIdentity    `json:"identity"`
	Build            *milvus.BuildResult        `json:"build,omitempty"`
	Verification     *milvus.VerificationReport `json:"verification,omitempty"`
	Publication      milvus.PublicationResult   `json:"publication"`
}

type Writer interface {
	GetAliasTarget(context.Context) (string, error)
	BuildCollection(context.Context, string, *milvus.PreparedImport) (*milvus.BuildResult, error)
	VerifyCollection(context.Context, *milvus.BuildResult, *milvus.PreparedImport) (*milvus.VerifiedTarget, error)
	PublishVerified(context.Context, string, *milvus.VerifiedTarget) (milvus.PublicationResult, error)
}

var _ Writer = (*milvus.Client)(nil)

func Rebuild(ctx context.Context, source indexing.Source, embedder vectorization.BatchEmbedder, writer Writer, options Options, report func(Result) error) (*Result, error) {
	result := &Result{
		Stage:         "preflight",
		NewCollection: options.NewCollection,
		Publication: milvus.PublicationResult{
			State: milvus.PublicationNotAttempted,
		},
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, fmt.Errorf("%w: overall deadline is required", milvus.ErrInput)
	}
	if source == nil || embedder == nil || writer == nil || report == nil {
		return result, fmt.Errorf("%w: dependencies and reporter are required", milvus.ErrInput)
	}
	for _, value := range []string{options.ExpectedDatabase, options.ExpectedServerUUID} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return result, fmt.Errorf("%w: invalid expected source identity", milvus.ErrInput)
		}
	}
	if !regexp.MustCompile(`^rule_chunks_vec_v1_[0-9a-f]{32}$`).MatchString(options.NewCollection) {
		return result, fmt.Errorf("%w: invalid new collection name", milvus.ErrInput)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(options.ExpectedDigest) {
		return result, fmt.Errorf("%w: invalid expected model digest", milvus.ErrInput)
	}

	advance := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		result.Stage = stage
		if err := report(*result); err != nil {
			return ErrEvidence
		}
		return nil
	}

	if err := advance("alias-read"); err != nil {
		return result, err
	}
	old, err := writer.GetAliasTarget(ctx)
	if err != nil {
		return result, err
	}
	if old == options.NewCollection {
		return result, fmt.Errorf("%w: new target is already published", milvus.ErrInput)
	}
	result.OldCollection = old

	if err := advance("export"); err != nil {
		return result, err
	}
	snapshot, err := source.ExportCurrent(ctx)
	if err != nil {
		return result, err
	}

	if err := advance("source-check"); err != nil {
		return result, err
	}
	if snapshot == nil || snapshot.SourceDatabase != options.ExpectedDatabase || snapshot.SourceServerUUID != options.ExpectedServerUUID {
		return result, fmt.Errorf("%w: source identity differs", indexing.ErrInvalidSource)
	}
	result.SourceDatabase = snapshot.SourceDatabase
	result.SourceServerUUID = snapshot.SourceServerUUID
	result.CapturedAt = snapshot.CapturedAt
	result.Count = len(snapshot.Documents)

	if err := advance("vectorize"); err != nil {
		return result, err
	}
	vectors, err := vectorization.Vectorize(ctx, snapshot, embedder)
	if err != nil {
		return result, err
	}
	result.Identity = vectors.Identity

	if err := advance("prepare-import"); err != nil {
		return result, err
	}
	prepared, err := milvus.PrepareImport(ctx, vectors, options.ExpectedDigest)
	if err != nil {
		return result, err
	}
	result.CapturedAt = prepared.CapturedAt
	result.Count = len(prepared.Records)

	if err := advance("build"); err != nil {
		return result, err
	}
	result.Build, err = writer.BuildCollection(
		ctx, options.NewCollection, prepared,
	)
	if err != nil {
		return result, err
	}
	if result.Build == nil {
		return result, fmt.Errorf("%w: missing build result", milvus.ErrResponse)
	}

	if err := advance("verify"); err != nil {
		return result, err
	}
	verified, err := writer.VerifyCollection(ctx, result.Build, prepared)
	if err != nil {
		return result, err
	}
	if verified == nil {
		return result, fmt.Errorf("%w: missing verification token", milvus.ErrResponse)
	}
	verification := verified.Report()
	result.Verification = &verification

	if err := advance("publish"); err != nil {
		return result, err
	}
	result.Publication, err = writer.PublishVerified(ctx, old, verified)
	if err != nil {
		return result, err
	}
	if result.Publication.State != milvus.PublicationApplied {
		return result, fmt.Errorf("%w: publication was not observed", milvus.ErrStateUnknown)
	}

	if err := advance("complete"); err != nil {
		return result, err
	}
	return result, nil
}
