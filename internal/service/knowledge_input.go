package service

import (
	"time"
	"video-support-agent/internal/model"
)

type CreateKnowledgeDraftInput struct {
	DocumentKey string
	Version     string
	CategoryID  uint64

	Title     string
	Content   string
	SourceURL string

	EffectiveAt *time.Time
	ExpiresAt   *time.Time

	Articles []CreateRuleArticleInput
}

type CreateKnowledgeRevisionInput struct {
	DocumentID uint64
	Version    string

	RevisionType      model.RuleRevisionType
	RevisionNote      string
	RevisionSourceURL *string

	CategoryID uint64
	Title      string
	Content    string
	SourceURL  string

	EffectiveAt *time.Time
	ExpiresAt   *time.Time

	Articles []CreateRuleArticleInput
}

type EditKnowledgeDraftInput struct {
	VersionID               uint64
	ExpectedContentRevision uint64

	CategoryID uint64
	Title      string
	Content    string
	SourceURL  string

	EffectiveAt *time.Time
	ExpiresAt   *time.Time

	Articles []CreateRuleArticleInput
}

type VerifyKnowledgeDraftInput struct {
	VersionID               uint64
	ExpectedContentRevision uint64

	VerificationNote string
	EvidenceURL      *string
}

type PublishKnowledgeVersionInput struct {
	DocumentID                  uint64
	VersionID                   uint64
	ExpectedPublicationRevision uint64
}

type WithdrawKnowledgeVersionInput struct {
	DocumentID                  uint64
	VersionID                   uint64
	ExpectedPublicationRevision uint64
}

type CreateRuleArticleInput struct {
	SectionPath   string
	ArticleNumber string
	Content       string
}
