package evaluation

import "fmt"

func validateCaseShape(value any) error {
	root, err := requireObject(
		value,
		"case",
		[]string{
			"id",
			"schema_version",
			"suite",
			"question",
			"corpus_id",
			"filters",
			"top_k",
			"expected_intent",
			"expected_documents",
			"expected_tools",
			"reference_answer",
			"expected_citations",
			"expected_handoff",
			"required_points",
			"forbidden_claims",
		},
		nil,
		"expected_handoff",
	)
	if err != nil {
		return err
	}

	if _, err := requireObject(
		root["filters"],
		"filters",
		nil,
		[]string{"document_key", "version_key", "category_key"},
		"",
	); err != nil {
		return err
	}

	for _, name := range []string{"expected_documents", "required_points", "forbidden_claims"} {
		items, err := requireArray(root[name], name)
		if err != nil {
			return err
		}
		for i, item := range items {
			if _, ok := item.(string); !ok {
				return fieldError(fmt.Sprintf("%s[%d]", name, i), "must be a string")
			}
		}
	}

	tools, err := requireArray(root["expected_tools"], "expected_tools")
	if err != nil {
		return err
	}
	for i, value := range tools {
		path := fmt.Sprintf("expected_tools[%d]", i)
		tool, err := requireObject(
			value,
			path,
			[]string{"name", "arguments_match", "expected_status", "expected_business_effect"},
			nil,
			"",
		)
		if err != nil {
			return err
		}
		if _, ok := tool["arguments_match"].(map[string]any); !ok {
			return fieldError(path+".arguments_match", "must be an object")
		}
	}

	citations, err := requireArray(root["expected_citations"], "expected_citations")
	if err != nil {
		return err
	}
	for i, value := range citations {
		path := fmt.Sprintf("expected_citations[%d]", i)
		if _, err := requireObject(value, path, []string{"document_key", "version_key", "article_key"}, nil, ""); err != nil {
			return err
		}
	}

	return nil
}

func requireObject(value any, path string, required []string, optional []string, nullable string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fieldError(path, "must be an object")
	}

	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = struct{}{}
		if _, exists := object[key]; !exists {
			return nil, fieldError(path+"."+key, "required field is missing")
		}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}

	for key, item := range object {
		if _, exists := allowed[key]; !exists {
			return nil, fieldError(path, "unknown field")
		}
		if item == nil && key != nullable {
			return nil, fieldError(path+"."+key, "null is forbidden")
		}
	}
	return object, nil
}

func requireArray(value any, path string) ([]any, error) {
	array, ok := value.([]any)
	if !ok {
		return nil, fieldError(path, "must be an array")
	}
	return array, nil
}
