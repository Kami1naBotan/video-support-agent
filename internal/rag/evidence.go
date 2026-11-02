package rag

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/rulesource"
)

func BuildEvidence(ctx context.Context, items []retrieval.HybridItem, at time.Time) (EvidenceSet, error) {
	if err := ctx.Err(); err != nil {
		return EvidenceSet{}, err
	}

	if items == nil || len(items) > ChatTopK {
		return EvidenceSet{}, fmt.Errorf("%w: invalid article count", ErrInvalidEvidence)
	}
	if !validEvidenceTime(at) {
		return EvidenceSet{}, fmt.Errorf("%w: invalid retrieval time", ErrInvalidEvidence)
	}
	at = at.UTC()

	result := EvidenceSet{
		rules: make([]EvidenceRule, 0, len(items)),
		stats: EvidenceStats{InputCount: len(items)},
	}

	seenArticles := make(map[[3]uint64]struct{}, len(items))
	seenChunks := make(map[uint64]struct{})
	totalBytes := 0

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return EvidenceSet{}, err
		}

		chunk := item.Rule.Chunk

		if chunk.DocumentID == 0 || chunk.VersionID == 0 || chunk.ArticleID == 0 || chunk.CategoryID == 0 || chunk.ChunkID == 0 || chunk.ArticleOrder == 0 {
			return EvidenceSet{}, fmt.Errorf("%w: invalid article identity", ErrInvalidEvidence)
		}

		key := [3]uint64{chunk.DocumentID, chunk.VersionID, chunk.ArticleID}
		if _, exists := seenArticles[key]; exists {
			return EvidenceSet{}, fmt.Errorf("%w: duplicate article", ErrInvalidEvidence)
		}
		seenArticles[key] = struct{}{}

		if len(item.ChunkIDs) == 0 {
			return EvidenceSet{}, fmt.Errorf("%w: missing fragment provenance", ErrInvalidEvidence)
		}
		if len(item.ChunkIDs) > retrieval.MaxCandidates-len(seenChunks) {
			return EvidenceSet{}, fmt.Errorf("%w: %w", ErrInvalidEvidence, ErrEvidenceLimit)
		}

		chunkIDs := make([]string, 0, len(item.ChunkIDs))
		hasRepresentative := false

		for _, id := range item.ChunkIDs {
			if id == 0 {
				return EvidenceSet{}, fmt.Errorf("%w: invalid fragment identity", ErrInvalidEvidence)
			}
			if _, exists := seenChunks[id]; exists {
				return EvidenceSet{}, fmt.Errorf("%w: repeated fragment identity", ErrInvalidEvidence)
			}

			seenChunks[id] = struct{}{}
			chunkIDs = append(chunkIDs, strconv.FormatUint(id, 10))
			if id == chunk.ChunkID {
				hasRepresentative = true
			}
		}

		if !hasRepresentative {
			return EvidenceSet{}, fmt.Errorf("%w: representative fragment is missing", ErrInvalidEvidence)
		}

		rowBytes := 0
		metadataValid := true

		for _, text := range []string{chunk.Version, chunk.Title, chunk.SourceURL, chunk.SectionPath, chunk.ArticleNumber, chunk.Content, item.Rule.ArticleContent} {
			if len(text) > retrieval.MaxHydrationRowBytes-rowBytes {
				return EvidenceSet{}, fmt.Errorf("%w: %w", ErrInvalidEvidence, ErrEvidenceLimit)
			}
			rowBytes += len(text)

			if !utf8.ValidString(text) {
				metadataValid = false
			}
		}

		if rowBytes > retrieval.MaxHydrationTotalBytes-totalBytes {
			return EvidenceSet{}, fmt.Errorf("%w: %w", ErrInvalidEvidence, ErrEvidenceLimit)
		}
		totalBytes += rowBytes

		if !metadataValid ||
			strings.TrimSpace(chunk.Version) == "" ||
			strings.TrimSpace(chunk.Title) == "" ||
			strings.TrimSpace(item.Rule.ArticleContent) == "" ||
			utf8.RuneCountInString(chunk.Version) > 64 ||
			utf8.RuneCountInString(chunk.Title) > 255 ||
			utf8.RuneCountInString(chunk.SectionPath) > retrieval.MaxSectionRunes ||
			utf8.RuneCountInString(chunk.ArticleNumber) > retrieval.MaxArticleNumberRunes {
			result.stats.InvalidMetadataCount++
			continue
		}

		sourceURL := strings.TrimSpace(chunk.SourceURL)
		if err := rulesource.ValidateOfficialURL(sourceURL); err != nil {
			result.stats.InvalidSourceCount++
			continue
		}

		if !validEvidenceTime(chunk.EffectiveAt) || chunk.EffectiveAt.After(at) {
			result.stats.InactiveCount++
			continue
		}

		var expiresAt *time.Time
		if chunk.ExpiresAt != nil {
			if !validEvidenceTime(*chunk.ExpiresAt) || !chunk.ExpiresAt.After(at) {
				result.stats.InactiveCount++
				continue
			}

			value := chunk.ExpiresAt.UTC()
			expiresAt = &value
		}

		citation := model.RuleCitation{
			RefID: "R" + strconv.Itoa(len(result.rules)+1),

			DocumentID: strconv.FormatUint(chunk.DocumentID, 10),
			VersionID:  strconv.FormatUint(chunk.VersionID, 10),
			ArticleID:  strconv.FormatUint(chunk.ArticleID, 10),
			CategoryID: strconv.FormatUint(chunk.CategoryID, 10),
			ChunkID:    strconv.FormatUint(chunk.ChunkID, 10),
			ChunkIDs:   chunkIDs,

			Version:       chunk.Version,
			Title:         chunk.Title,
			SectionPath:   chunk.SectionPath,
			ArticleNumber: chunk.ArticleNumber,
			SourceURL:     sourceURL,

			EffectiveAt: chunk.EffectiveAt.UTC(),
			ExpiresAt:   expiresAt,
			RetrievedAt: at,
		}

		result.rules = append(result.rules, EvidenceRule{
			Citation:       citation,
			ArticleContent: item.Rule.ArticleContent,
		})
	}

	if err := ctx.Err(); err != nil {
		return EvidenceSet{}, err
	}

	result.stats.AcceptedCount = len(result.rules)
	return result, nil
}

func validEvidenceTime(value time.Time) bool {
	value = value.UTC()
	return !value.IsZero() && value.Year() >= 1 && value.Year() <= 9999 && value.Nanosecond()%1_000_000 == 0
}
