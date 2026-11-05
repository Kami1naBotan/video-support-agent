package knowledgeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/knowledgechunk"
)

var (
	ErrProjectionConflict = errors.New("article projection conflict")
	ErrProjectionLimit    = errors.New("article projection limit exceeded")
)

type DocumentRevision struct {
	DocumentID          uint64  `json:"document_id"`
	PublicationRevision uint64  `json:"publication_revision"`
	CurrentVersionID    *uint64 `json:"current_version_id"`
}

type BuildSnapshot struct {
	SourceEpoch    uint64
	ChunkStrategy  string
	Prepared       *indexing.PreparedSnapshot
	Revisions      []DocumentRevision
	ManifestSHA256 string
}

func NewBuildSnapshot(
	ctx context.Context,
	epoch uint64,
	prepared *indexing.PreparedSnapshot,
	revisions []DocumentRevision,
) (*BuildSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if epoch == 0 || prepared == nil {
		return nil, ErrBuildState
	}

	sort.Slice(prepared.Documents, func(i, j int) bool {
		return prepared.Documents[i].ID < prepared.Documents[j].ID
	})
	sort.Slice(revisions, func(i, j int) bool {
		return revisions[i].DocumentID < revisions[j].DocumentID
	})

	for i, revision := range revisions {
		if revision.DocumentID == 0 ||
			(i > 0 && revisions[i-1].DocumentID == revision.DocumentID) {
			return nil, ErrBuildState
		}
	}

	manifest := struct {
		Schema    int                        `json:"schema"`
		Epoch     uint64                     `json:"epoch"`
		Strategy  string                     `json:"strategy"`
		Prepared  *indexing.PreparedSnapshot `json:"prepared"`
		Revisions []DocumentRevision         `json:"revisions"`
	}{
		Schema:    1,
		Epoch:     epoch,
		Strategy:  knowledgechunk.Strategy,
		Prepared:  prepared,
		Revisions: revisions,
	}

	hash := sha256.New()
	if err := json.NewEncoder(hash).Encode(manifest); err != nil {
		return nil, ErrBuildState
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &BuildSnapshot{
		SourceEpoch:    epoch,
		ChunkStrategy:  knowledgechunk.Strategy,
		Prepared:       prepared,
		Revisions:      revisions,
		ManifestSHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}
