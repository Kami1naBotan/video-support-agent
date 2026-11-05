package knowledgeworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/knowledgechunk"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/vectorization"
)

const (
	PhasePrepared     = "prepared"
	PhaseESCreate     = "es_create"
	PhaseESBulk       = "es_bulk"
	PhaseESVerify     = "es_verify"
	PhaseMilvusBuild  = "milvus_build"
	PhaseMilvusVerify = "milvus_verify"
	PhaseSealed       = "sealed"

	VectorNormalization = "provider_float32_cosine_v1"
)

var (
	ErrDualExternal  = errors.New("dual index external operation failed")
	ErrDualPermanent = errors.New("dual index input or identity invalid")
	ErrDualTimeout   = errors.New("dual index operation timed out")

	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	suffixPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type IdentityEmbedder interface {
	vectorization.BatchEmbedder
	InspectIdentity(context.Context) (embedding.ModelIdentity, error)
}

type ESBuildWriter interface {
	CreateIndex(context.Context, string, []byte) error
	BulkIndex(context.Context, string, []indexing.EncodedDocument) error
	VerifyIndex(context.Context, string, []indexing.EncodedDocument) error
}

type MilvusBuildWriter interface {
	BuildCollection(context.Context, string, *milvus.PreparedImport) (*milvus.BuildResult, error)
	VerifyCollection(context.Context, *milvus.BuildResult, *milvus.PreparedImport) (*milvus.VerifiedTarget, error)
}

type GenerationStore interface {
	BuildStore
	PrepareSnapshot(context.Context, BuildClaim) (*BuildSnapshot, error)
	StartGeneration(context.Context, BuildClaim, *BuildSnapshot, embedding.ModelIdentity) error
	AdvanceGeneration(context.Context, BuildClaim, string, string, string) error
}

type DualBuildOptions struct {
	ExpectedDatabase   string
	ExpectedServerUUID string
	ExpectedDigest     string
	ESDefinition       []byte
}

type VerifiedGeneration struct {
	Claim    BuildClaim
	Snapshot *BuildSnapshot
	Identity embedding.ModelIdentity
}

type DualBuilder struct {
	store    GenerationStore
	embedder IdentityEmbedder
	es       ESBuildWriter
	vector   MilvusBuildWriter
	options  DualBuildOptions
}

func NewDualBuilder(store GenerationStore, embedder IdentityEmbedder, es ESBuildWriter, vector MilvusBuildWriter, options DualBuildOptions) (*DualBuilder, error) {
	if store == nil || embedder == nil || es == nil || vector == nil {
		return nil, ErrBuildConfig
	}
	for _, value := range []string{
		options.ExpectedDatabase,
		options.ExpectedServerUUID,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return nil, ErrBuildConfig
		}
	}
	if !digestPattern.MatchString(options.ExpectedDigest) || len(options.ESDefinition) == 0 || len(options.ESDefinition) > 4<<20 || !json.Valid(options.ESDefinition) {
		return nil, ErrBuildConfig
	}

	options.ESDefinition = bytes.Clone(options.ESDefinition)
	return &DualBuilder{
		store:    store,
		embedder: embedder,
		es:       es,
		vector:   vector,
		options:  options,
	}, nil
}

func checkedBuildSnapshot(ctx context.Context, claim BuildClaim, input *BuildSnapshot, options DualBuildOptions) (*BuildSnapshot, *indexing.Snapshot, error) {
	suffix := strings.ReplaceAll(claim.GenerationID, "-", "")
	if !suffixPattern.MatchString(suffix) ||
		claim.ESIndexName != "knowledge_g_"+suffix ||
		claim.MilvusCollection != "rule_chunks_vec_v1_"+suffix {
		return nil, nil, ErrBuildState
	}
	if input == nil || input.Prepared == nil ||
		input.SourceEpoch != claim.SourceEpoch ||
		input.ChunkStrategy != knowledgechunk.Strategy ||
		!digestPattern.MatchString(input.ManifestSHA256) {
		return nil, nil, ErrBuildState
	}

	p := input.Prepared
	if p.SourceDatabase != options.ExpectedDatabase ||
		p.SourceServerUUID != options.ExpectedServerUUID ||
		p.Documents == nil ||
		len(p.Documents) > indexing.MaxExportDocuments ||
		len(input.Revisions) > indexing.MaxExportDocuments {
		return nil, nil, ErrDualPermanent
	}

	revisions := make([]DocumentRevision, len(input.Revisions))
	byDocument := make(map[string]DocumentRevision, len(revisions))
	for i, revision := range input.Revisions {
		if revision.DocumentID == 0 {
			return nil, nil, ErrBuildState
		}
		if revision.CurrentVersionID != nil {
			versionID := *revision.CurrentVersionID
			if versionID == 0 {
				return nil, nil, ErrBuildState
			}
			revision.CurrentVersionID = &versionID
		}
		key := strconv.FormatUint(revision.DocumentID, 10)
		if _, exists := byDocument[key]; exists {
			return nil, nil, ErrBuildState
		}
		revisions[i] = revision
		byDocument[key] = revision
	}

	raw := &indexing.Snapshot{
		SourceDatabase:   p.SourceDatabase,
		SourceServerUUID: p.SourceServerUUID,
		CapturedAt:       p.CapturedAt,
		Documents:        make([]indexing.Document, len(p.Documents)),
	}
	for i, encoded := range p.Documents {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(encoded.JSON) > indexing.MaxIndexDocumentJSONBytes {
			return nil, nil, ErrProjectionLimit
		}
		var document indexing.Document
		if err := json.Unmarshal(encoded.JSON, &document); err != nil || document.ChunkID != encoded.ID {
			return nil, nil, ErrBuildState
		}

		revision, ok := byDocument[document.DocumentID]
		if !ok || revision.CurrentVersionID == nil ||
			strconv.FormatUint(*revision.CurrentVersionID, 10) != document.VersionID ||
			strconv.FormatUint(revision.PublicationRevision, 10) != document.PublicationRevision {
			return nil, nil, ErrBuildState
		}
		raw.Documents[i] = document
	}

	canonical, err := indexing.PrepareSnapshot(ctx, raw)
	if err != nil {
		return nil, nil, err
	}
	checked, err := NewBuildSnapshot(ctx, claim.SourceEpoch, canonical, revisions)
	if err != nil {
		return nil, nil, err
	}
	if checked.ManifestSHA256 != input.ManifestSHA256 {
		return nil, nil, ErrBuildState
	}
	return checked, raw, nil
}

func validBuildIdentity(identity embedding.ModelIdentity, digest string) bool {
	return identity.Model == embedding.ModelName &&
		identity.Digest == digest &&
		digestPattern.MatchString(identity.Digest) &&
		identity.Dimensions == embedding.Dimensions &&
		identity.ContextLength == embedding.ContextLength &&
		identity.InputPolicy == embedding.InputPolicy &&
		identity.ServerVersion != "" &&
		identity.ServerVersion == strings.TrimSpace(identity.ServerVersion)
}

func externalCall(ctx context.Context, operation func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	err := operation(bounded)
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if bounded.Err() != nil {
		return ErrDualTimeout
	}
	return err
}

type pinnedEmbedder struct {
	inner    IdentityEmbedder
	identity embedding.ModelIdentity
}

func (p pinnedEmbedder) EmbedWithIdentity(ctx context.Context, texts []string) (embedding.BatchResult, error) {
	var result embedding.BatchResult
	err := externalCall(ctx, func(callCtx context.Context) error {
		var err error
		result, err = p.inner.EmbedWithIdentity(callCtx, texts)
		return err
	})
	if err != nil {
		return embedding.BatchResult{}, err
	}
	if result.Identity != p.identity {
		return embedding.BatchResult{}, ErrDualPermanent
	}
	return result, nil
}

func (b *DualBuilder) Build(ctx context.Context, claim BuildClaim) (*VerifiedGeneration, error) {
	var result *VerifiedGeneration

	err := RunWithBuildLease(ctx, b.store, claim, func(workCtx context.Context) error {
		input, err := b.store.PrepareSnapshot(workCtx, claim)
		if err != nil {
			return err
		}
		snapshot, raw, err := checkedBuildSnapshot(workCtx, claim, input, b.options)
		if err != nil {
			return err
		}

		var identity embedding.ModelIdentity
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			var err error
			identity, err = b.embedder.InspectIdentity(callCtx)
			return err
		}); err != nil {
			return err
		}
		if !validBuildIdentity(identity, b.options.ExpectedDigest) {
			return ErrDualPermanent
		}

		if err := b.store.StartGeneration(workCtx, claim, snapshot, identity); err != nil {
			return err
		}

		advance := func(from, to string) error {
			return b.store.AdvanceGeneration(workCtx, claim, snapshot.ManifestSHA256, from, to)
		}

		if len(snapshot.Prepared.Documents) == 0 {
			if err := advance(PhasePrepared, PhaseSealed); err != nil {
				return err
			}
			result = &VerifiedGeneration{Claim: claim, Snapshot: snapshot, Identity: identity}
			return nil
		}

		vectors, err := vectorization.Vectorize(workCtx, raw, pinnedEmbedder{inner: b.embedder, identity: identity})
		if err != nil {
			return err
		}
		prepared, err := milvus.PrepareImport(workCtx, vectors, b.options.ExpectedDigest)
		if err != nil {
			return err
		}

		if err := advance(PhasePrepared, PhaseESCreate); err != nil {
			return err
		}
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			return b.es.CreateIndex(callCtx, claim.ESIndexName, b.options.ESDefinition)
		}); err != nil {
			return err
		}

		if err := advance(PhaseESCreate, PhaseESBulk); err != nil {
			return err
		}
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			return b.es.BulkIndex(callCtx, claim.ESIndexName, snapshot.Prepared.Documents)
		}); err != nil {
			return err
		}

		if err := advance(PhaseESBulk, PhaseESVerify); err != nil {
			return err
		}
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			return b.es.VerifyIndex(callCtx, claim.ESIndexName, snapshot.Prepared.Documents)
		}); err != nil {
			return err
		}

		if err := advance(PhaseESVerify, PhaseMilvusBuild); err != nil {
			return err
		}
		var built *milvus.BuildResult
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			var err error
			built, err = b.vector.BuildCollection(callCtx, claim.MilvusCollection, prepared)
			return err
		}); err != nil {
			return err
		}
		if built == nil || built.Collection != claim.MilvusCollection ||
			built.CollectionID <= 0 || built.Inserted != len(prepared.Records) ||
			built.Stage != "ready-for-verification" {
			return ErrBuildState
		}

		if err := advance(PhaseMilvusBuild, PhaseMilvusVerify); err != nil {
			return err
		}
		var verified *milvus.VerifiedTarget
		if err := externalCall(workCtx, func(callCtx context.Context) error {
			var err error
			verified, err = b.vector.VerifyCollection(callCtx, built, prepared)
			return err
		}); err != nil {
			return err
		}
		if verified == nil {
			return ErrBuildState
		}
		report := verified.Report()
		if report.Collection != claim.MilvusCollection ||
			report.CollectionID != built.CollectionID ||
			report.Count != len(prepared.Records) ||
			report.Identity != identity {
			return ErrBuildState
		}

		if err := advance(PhaseMilvusVerify, PhaseSealed); err != nil {
			return err
		}
		result = &VerifiedGeneration{Claim: claim, Snapshot: snapshot, Identity: identity}
		return nil
	})
	if err != nil {
		return nil, safeDualError(err)
	}
	if result == nil {
		return nil, ErrBuildState
	}
	return result, nil
}

func safeDualError(err error) error {
	if err == nil {
		return nil
	}

	for _, known := range []error{
		ErrBuildStore,
		ErrBuildConfig,
		ErrBuildState,
		ErrBuildLeaseLost,
		ErrBuildObsolete,
		ErrBuildCapacity,
		ErrBuildExhausted,
		context.Canceled,
		ErrProjectionConflict,
		ErrProjectionLimit,
		ErrDualPermanent,
		ErrDualTimeout,
		ErrDualExternal,
	} {
		if errors.Is(err, known) {
			return known
		}
	}

	if errors.Is(err, indexing.ErrExportLimitExceeded) {
		return ErrProjectionLimit
	}

	for _, permanent := range []error{
		embedding.ErrModelMismatch,
		embedding.ErrInput,
		embedding.ErrInputTooLarge,
		embedding.ErrContextLength,
		embedding.ErrRejected,
		embedding.ErrResponse,
		indexing.ErrInvalidSource,
		milvus.ErrInput,
		milvus.ErrRejected,
		milvus.ErrResponse,
		elasticsearch.ErrRejected,
		elasticsearch.ErrResponse,
	} {
		if errors.Is(err, permanent) {
			return ErrDualPermanent
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return ErrDualTimeout
	}

	for _, transient := range []error{
		embedding.ErrUnavailable,
		milvus.ErrUnavailable,
		milvus.ErrStateUnknown,
		elasticsearch.ErrUnavailable,
		elasticsearch.ErrStateUnknown,
	} {
		if errors.Is(err, transient) {
			return ErrDualExternal
		}
	}

	return ErrBuildState
}
