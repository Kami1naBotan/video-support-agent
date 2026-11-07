package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	MaxDatasetBytes = 8 * 1024 * 1024
	MaxLineBytes    = 256 * 1024
	MaxCases        = 1000
	MaxJSONDepth    = 64
)

func LoadCases(r io.Reader) ([]Case, error) {
	if r == nil {
		return nil, errors.New("evaluation JSONL: reader is required")
	}

	data, err := io.ReadAll(io.LimitReader(r, MaxDatasetBytes+1))
	if err != nil {
		return nil, errors.New("evaluation JSONL: input read failed")
	}
	if len(data) > MaxDatasetBytes {
		return nil, errors.New("evaluation JSONL: dataset exceeds size limit")
	}
	if len(data) == 0 {
		return nil, lineError(1, errors.New("dataset is empty"))
	}

	lines := bytes.SplitN(data, []byte{'\n'}, MaxCases+1)
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}

	cases := make([]Case, 0, len(lines))
	seen := make(map[string]struct{})

	for i, raw := range lines {
		line := i + 1
		if line > MaxCases {
			return nil, lineError(line, errors.New("too many samples"))
		}

		raw = bytes.TrimSuffix(raw, []byte{'\r'})
		if len(raw) > MaxLineBytes {
			return nil, lineError(line, errors.New("line exceeds size limit"))
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil, lineError(line, errors.New("blank lines are forbidden"))
		}
		if !utf8.Valid(raw) {
			return nil, lineError(line, errors.New("invalid UTF-8"))
		}

		c, err := decodeCase(raw)
		if err != nil {
			return nil, lineError(line, err)
		}
		if _, exists := seen[c.ID]; exists {
			return nil, lineError(line, errors.New("duplicate case ID"))
		}

		seen[c.ID] = struct{}{}
		cases = append(cases, c)
	}
	return cases, nil
}

func lineError(line int, err error) error {
	return fmt.Errorf("evaluation JSONL line %d: %w", line, err)
}

func decodeCase(raw []byte) (Case, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return Case{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Case{}, errors.New("trailing data is forbidden")
	}

	if err := validateCaseShape(value); err != nil {
		return Case{}, err
	}

	normalized, err := json.Marshal(value)
	if err != nil {
		return Case{}, errors.New("cannot encode parsed JSON")
	}

	var c Case
	if err := json.Unmarshal(normalized, &c); err != nil {
		return Case{}, errors.New("field type or numeric range is invalid")
	}
	if err := ValidateCase(c); err != nil {
		return Case{}, err
	}
	return c, nil
}

func readJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, errors.New("JSON nesting exceeds depth limit")
	}

	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("invalid JSON syntax")
	}

	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}

	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, errors.New("invalid JSON object")
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("object key must be a string")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate JSON object key")
			}

			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}

		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid JSON object ending")
		}
		return object, nil

	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := readJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}

		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid JSON array ending")
		}
		return array, nil

	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}
