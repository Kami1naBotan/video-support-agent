package evaluation

import "encoding/json"

const (
	SchemaVersion = 1

	SuiteRetrieval = "retrieval"
	SuiteAgent     = "agent"

	MaxKeyBytes           = 128
	MaxTopK               = 50
	MaxQuestionBytes      = 32 * 1024
	MaxReferenceBytes     = 64 * 1024
	MaxListTextBytes      = 512
	MaxExpectedDocuments  = 100
	MaxExpectedCitations  = 100
	MaxExpectedTools      = 5
	MaxReviewPoints       = 100
	MaxArgumentFields     = 32
	MaxArgumentMatchBytes = 8 * 1024
)

type Case struct {
	ID                string            `json:"id"`
	SchemaVersion     int               `json:"schema_version"`
	Suite             string            `json:"suite"`
	Question          string            `json:"question"`
	CorpusID          string            `json:"corpus_id"`
	Filters           *Filters          `json:"filters"`
	TopK              int               `json:"top_k"`
	ExpectedIntent    string            `json:"expected_intent"`
	ExpectedDocuments []string          `json:"expected_documents"`
	ExpectedTools     []ToolExpectation `json:"expected_tools"`
	ReferenceAnswer   string            `json:"reference_answer"`
	ExpectedCitations []CitationKey     `json:"expected_citations"`
	ExpectedHandoff   *bool             `json:"expected_handoff"`
	RequiredPoints    []string          `json:"required_points"`
	ForbiddenClaims   []string          `json:"forbidden_claims"`
}

type Filters struct {
	DocumentKey string `json:"document_key,omitempty"`
	VersionKey  string `json:"version_key,omitempty"`
	CategoryKey string `json:"category_key,omitempty"`
}

type CitationKey struct {
	DocumentKey string `json:"document_key"`
	VersionKey  string `json:"version_key"`
	ArticleKey  string `json:"article_key"`
}

type ToolExpectation struct {
	Name                   string                     `json:"name"`
	ArgumentsMatch         map[string]json.RawMessage `json:"arguments_match"`
	ExpectedStatus         string                     `json:"expected_status"`
	ExpectedBusinessEffect string                     `json:"expected_business_effect"`
}
