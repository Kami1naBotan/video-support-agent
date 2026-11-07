package evaluation

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

func ValidateCase(c Case) error {
	if c.SchemaVersion != SchemaVersion {
		return fieldError("schema_version", "unsupported version")
	}
	if err := validateKey("id", c.ID); err != nil {
		return err
	}
	if err := validateKey("corpus_id", c.CorpusID); err != nil {
		return err
	}
	if !oneOf(c.Suite, SuiteRetrieval, SuiteAgent) {
		return fieldError("suite", "unsupported suite")
	}
	if err := validateText("question", c.Question, MaxQuestionBytes); err != nil {
		return err
	}
	if err := validateText("reference_answer", c.ReferenceAnswer, MaxReferenceBytes); err != nil {
		return err
	}
	if c.TopK < 1 || c.TopK > MaxTopK {
		return fieldError("top_k", "must be between 1 and 50")
	}

	if c.Filters == nil {
		return fieldError("filters", "must be an object")
	}
	for _, item := range []struct {
		path  string
		value string
	}{
		{"filters.document_key", c.Filters.DocumentKey},
		{"filters.version_key", c.Filters.VersionKey},
		{"filters.category_key", c.Filters.CategoryKey},
	} {
		if item.value != "" {
			if err := validateKey(item.path, item.value); err != nil {
				return err
			}
		}
	}

	if err := validateStrings("expected_documents", c.ExpectedDocuments, MaxExpectedDocuments, true); err != nil {
		return err
	}
	if err := validateStrings("required_points", c.RequiredPoints, MaxReviewPoints, false); err != nil {
		return err
	}
	if err := validateStrings("forbidden_claims", c.ForbiddenClaims, MaxReviewPoints, false); err != nil {
		return err
	}
	if err := validateCitations(c); err != nil {
		return err
	}
	if err := validateTools(c.ExpectedTools); err != nil {
		return err
	}
	return validateSuite(c)
}

func validateSuite(c Case) error {
	if c.Suite == SuiteRetrieval {
		if c.ExpectedIntent != "retrieve" {
			return fieldError("expected_intent", "retrieval requires retrieve")
		}
		if c.ExpectedHandoff != nil {
			return fieldError("expected_handoff", "retrieval requires null")
		}
		if len(c.ExpectedTools) != 0 {
			return fieldError("expected_tools", "retrieval requires an empty array")
		}
		return nil
	}

	if !oneOf(c.ExpectedIntent, "answer", "clarify", "handoff", "confirmation_required", "ticket_created", "ticket_status", "failed") {
		return fieldError("expected_intent", "unsupported agent decision")
	}
	if c.ExpectedHandoff == nil {
		return fieldError("expected_handoff", "agent requires a boolean")
	}
	if *c.ExpectedHandoff != (c.ExpectedIntent == "handoff") {
		return fieldError("expected_handoff", "conflicts with expected_intent")
	}
	return nil
}

func validateCitations(c Case) error {
	if c.ExpectedCitations == nil {
		return fieldError("expected_citations", "must be an explicit array")
	}
	if len(c.ExpectedCitations) > MaxExpectedCitations {
		return fieldError("expected_citations", "too many items")
	}

	documents := make(map[string]struct{}, len(c.ExpectedDocuments))
	for _, key := range c.ExpectedDocuments {
		documents[key] = struct{}{}
	}

	seen := make(map[CitationKey]struct{}, len(c.ExpectedCitations))
	for i, citation := range c.ExpectedCitations {
		path := fmt.Sprintf("expected_citations[%d]", i)
		if err := validateKey(path+".document_key", citation.DocumentKey); err != nil {
			return err
		}
		if err := validateKey(path+".version_key", citation.VersionKey); err != nil {
			return err
		}
		if err := validateKey(path+".article_key", citation.ArticleKey); err != nil {
			return err
		}
		if _, ok := documents[citation.DocumentKey]; !ok {
			return fieldError(path+".document_key", "must belong to expected_documents")
		}
		if _, ok := seen[citation]; ok {
			return fieldError(path, "duplicate citation")
		}
		seen[citation] = struct{}{}
	}
	return nil
}

func validateTools(items []ToolExpectation) error {
	if items == nil {
		return fieldError("expected_tools", "must be an explicit array")
	}
	if len(items) > MaxExpectedTools {
		return fieldError("expected_tools", "too many items")
	}

	for i, item := range items {
		path := fmt.Sprintf("expected_tools[%d]", i)
		if !oneOf(item.Name, "search_rule", "get_rule_detail", "check_appeal_eligibility", "create_appeal_ticket", "query_ticket_status") {
			return fieldError(path+".name", "unsupported tool")
		}
		if !oneOf(item.ExpectedStatus, "succeeded", "rejected", "failed", "cancelled", "timed_out", "unknown") {
			return fieldError(path+".expected_status", "unsupported status")
		}
		if !oneOf(item.ExpectedBusinessEffect, "none", "unknown", "committed") {
			return fieldError(path+".expected_business_effect", "unsupported business effect")
		}
		if item.ArgumentsMatch == nil {
			return fieldError(path+".arguments_match", "must be an object")
		}
		if len(item.ArgumentsMatch) > MaxArgumentFields {
			return fieldError(path+".arguments_match", "too many fields")
		}

		totalBytes := 0
		for key, value := range item.ArgumentsMatch {
			if err := validateKey(path+".arguments_match", key); err != nil {
				return err
			}
			if len(value) > MaxArgumentMatchBytes {
				return fieldError(path+".arguments_match", "value is too large")
			}
			totalBytes += len(key) + len(value)
			if totalBytes > MaxArgumentMatchBytes {
				return fieldError(path+".arguments_match", "content is too large")
			}
			if !utf8.Valid(value) || !json.Valid(value) {
				return fieldError(path+".arguments_match", "values must contain valid UTF-8 JSON")
			}
		}
	}
	return nil
}

func validateStrings(path string, values []string, maxItems int, keys bool) error {
	if values == nil {
		return fieldError(path, "must be an explicit array")
	}
	if len(values) > maxItems {
		return fieldError(path, "too many items")
	}

	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		var err error
		if keys {
			err = validateKey(itemPath, value)
		} else {
			err = validateText(itemPath, value, MaxListTextBytes)
		}
		if err != nil {
			return err
		}
		if _, ok := seen[value]; ok {
			return fieldError(itemPath, "duplicate item")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateKey(path, value string) error {
	if len(value) == 0 || len(value) > MaxKeyBytes {
		return fieldError(path, "key length must be between 1 and 128 bytes")
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'a' && b <= 'z') ||
			(b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') ||
			b == '.' || b == '_' || b == '-' {
			continue
		}
		return fieldError(path, "key must use ASCII letters, digits, dot, underscore or hyphen")
	}
	return nil
}

func validateText(path, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return fieldError(path, "must be valid UTF-8")
	}
	if strings.TrimSpace(value) == "" {
		return fieldError(path, "must not be blank")
	}
	if len(value) > maxBytes {
		return fieldError(path, "text is too large")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func fieldError(path, reason string) error {
	return fmt.Errorf("evaluation case: %s: %s", path, reason)
}
