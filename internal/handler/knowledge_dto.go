package handler

import "time"

type createKnowledgeDraftRequest struct {
	DocumentKey string `json:"document_key"`
	Version     string `json:"version"`
	CategoryID  uint64 `json:"category_id"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceURL   string `json:"source_url"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Articles []createRuleArticleRequest `json:"articles"`
}

type createRuleArticleRequest struct {
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`
	Content       string `json:"content"`
}

type createKnowledgeDraftResponse struct {
	DocumentID uint64 `json:"document_id"`
	VersionID  uint64 `json:"version_id"`
	Version    string `json:"version"`
	Status     string `json:"status"`
}

type createKnowledgeRevisionRequest struct {
	Version           string  `json:"version"`
	RevisionType      string  `json:"revision_type"`
	RevisionNote      string  `json:"revision_note"`
	RevisionSourceURL *string `json:"revision_source_url"`

	CategoryID uint64 `json:"category_id"`
	Title      string `json:"title"`
	Content    string `json:"content"`
	SourceURL  string `json:"source_url"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Articles []createRuleArticleRequest `json:"articles"`
}

type knowledgeDraftDetailResponse struct {
	DocumentID        uint64  `json:"document_id"`
	VersionID         uint64  `json:"version_id"`
	Version           string  `json:"version"`
	CategoryID        uint64  `json:"category_id"`
	RevisionType      string  `json:"revision_type"`
	RevisionNote      string  `json:"revision_note"`
	RevisionSourceURL *string `json:"revision_source_url"`
	Title             string  `json:"title"`
	Content           string  `json:"content"`
	SourceURL         string  `json:"source_url"`
	Status            string  `json:"status"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Articles        []ruleArticleResponse          `json:"articles"`
	ContentRevision uint64                         `json:"content_revision"`
	Verification    *knowledgeVerificationResponse `json:"verification"`
}

type ruleArticleResponse struct {
	ArticleID     uint64 `json:"article_id"`
	ArticleOrder  uint32 `json:"article_order"`
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`
	Content       string `json:"content"`
}

type editKnowledgeDraftRequest struct {
	ExpectedContentRevision uint64 `json:"expected_content_revision"`
	CategoryID              uint64 `json:"category_id"`
	Title                   string `json:"title"`
	Content                 string `json:"content"`
	SourceURL               string `json:"source_url"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Articles []createRuleArticleRequest `json:"articles"`
}

type verifyKnowledgeDraftRequest struct {
	ExpectedContentRevision uint64  `json:"expected_content_revision"`
	VerificationNote        string  `json:"verification_note"`
	EvidenceURL             *string `json:"evidence_url"`
}

type knowledgeVerificationResponse struct {
	VerificationID          uint64    `json:"verification_id"`
	VerifiedContentRevision uint64    `json:"verified_content_revision"`
	VerifiedBy              uint64    `json:"verified_by"`
	VerifiedAt              time.Time `json:"verified_at"`
	VerificationNote        string    `json:"verification_note"`
	EvidenceURL             *string   `json:"evidence_url"`
	MatchesCurrentContent   bool      `json:"matches_current_content"`
}

type editKnowledgeDraftResponse struct {
	VersionID       uint64                         `json:"version_id"`
	ContentRevision uint64                         `json:"content_revision"`
	Verification    *knowledgeVerificationResponse `json:"verification"`
}

type verifyKnowledgeDraftResponse struct {
	VerificationID          uint64    `json:"verification_id"`
	VersionID               uint64    `json:"version_id"`
	VerifiedContentRevision uint64    `json:"verified_content_revision"`
	VerifiedBy              uint64    `json:"verified_by"`
	VerifiedAt              time.Time `json:"verified_at"`
}

type publishKnowledgeVersionRequest struct {
	VersionID                   uint64  `json:"version_id"`
	ExpectedPublicationRevision *uint64 `json:"expected_publication_revision"`
}

type publishKnowledgeVersionResponse struct {
	DocumentID          uint64    `json:"document_id"`
	VersionID           uint64    `json:"version_id"`
	PublicationRevision uint64    `json:"publication_revision"`
	PublishedAt         time.Time `json:"published_at"`
}

type withdrawKnowledgeVersionRequest struct {
	VersionID                   uint64  `json:"version_id"`
	ExpectedPublicationRevision *uint64 `json:"expected_publication_revision"`
}

type withdrawKnowledgeVersionResponse struct {
	DocumentID          uint64    `json:"document_id"`
	VersionID           uint64    `json:"version_id"`
	PublicationRevision uint64    `json:"publication_revision"`
	WithdrawnAt         time.Time `json:"withdrawn_at"`
}

type knowledgeCurrentDetailResponse struct {
	DocumentID uint64 `json:"document_id"`
	VersionID  uint64 `json:"version_id"`
	Version    string `json:"version"`
	CategoryID uint64 `json:"category_id"`

	Title     string `json:"title"`
	Content   string `json:"content"`
	SourceURL string `json:"source_url"`

	EffectiveAt *time.Time `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`

	Articles []ruleArticleResponse `json:"articles"`
}

type knowledgeDocumentStateResponse struct {
	DocumentID          uint64  `json:"document_id"`
	CurrentVersionID    *uint64 `json:"current_version_id"`
	PublicationRevision uint64  `json:"publication_revision"`
}
