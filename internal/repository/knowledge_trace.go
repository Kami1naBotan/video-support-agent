package repository

import (
	"context"
	"errors"

	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/observability"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func (r *KnowledgeBuildRepository) ReadBuildTraceParents(ctx context.Context, claim knowledgeworker.BuildClaim) ([]string, bool, error) {
	if r == nil || r.db == nil {
		return nil, false, errors.New("build trace source unavailable")
	}

	var parents []string
	err := r.db.Session(&gorm.Session{
		Logger: logger.Default.LogMode(logger.Silent),
	}).WithContext(ctx).
		Table("knowledge_build_events AS b").
		Joins("JOIN knowledge_index_attempts AS a "+
			"ON a.event_id = b.event_id "+
			"AND a.attempt = b.attempt "+
			"AND a.source_sha256 = b.source_sha256").
		Where("b.generation_id = ?", claim.GenerationID).
		Where("a.trace_parent <> ''").
		Order("b.event_id ASC, b.attempt ASC").
		Limit(observability.MaxAsyncLinks+1).
		Pluck("a.trace_parent", &parents).Error
	if err != nil {
		return nil, false, errors.New("build trace source unavailable")
	}

	truncated := len(parents) > observability.MaxAsyncLinks
	if truncated {
		parents = parents[:observability.MaxAsyncLinks]
	}
	return parents, truncated, nil
}
