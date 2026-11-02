package rag

import (
	"errors"

	"video-support-agent/internal/model"
)

const ChatTopK = 5

var (
	ErrInvalidEvidence = errors.New("invalid rule evidence")
	ErrEvidenceLimit   = errors.New("rule evidence exceeds read budget")
)

type EvidenceRule struct {
	Citation       model.RuleCitation
	ArticleContent string
}

type EvidenceStats struct {
	InputCount           int
	AcceptedCount        int
	InvalidMetadataCount int
	InvalidSourceCount   int
	InactiveCount        int
}

type EvidenceSet struct {
	rules []EvidenceRule
	stats EvidenceStats
}

func (s EvidenceSet) Rules() []EvidenceRule {
	result := make([]EvidenceRule, len(s.rules))
	for i, rule := range s.rules {
		result[i] = rule
		result[i].Citation = rule.Citation.Clone()
	}
	return result
}

func (s EvidenceSet) Stats() EvidenceStats {
	return s.stats
}
