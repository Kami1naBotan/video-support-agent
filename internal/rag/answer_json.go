package rag

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

func validAnswerJSON(raw []byte) bool {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}

	inString := false

	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '"':
			inString = !inString

		case inString && raw[i] == '\\':
			i++
			if raw[i] != 'u' {
				continue
			}

			unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 4

			if unit >= 0xDC00 && unit <= 0xDFFF {
				return false
			}

			if unit >= 0xD800 && unit <= 0xDBFF {
				if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
					return false
				}

				low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return false
				}
				i += 6
			}
		}
	}

	return true
}

func answerObject(raw json.RawMessage, fields ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))

	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrInvalidAnswer
	}

	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}

	object := make(map[string]json.RawMessage, len(fields))

	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] {
			return nil, ErrInvalidAnswer
		}
		if _, exists := object[name]; exists {
			return nil, ErrInvalidAnswer
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrInvalidAnswer
		}
		object[name] = value
	}

	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, ErrInvalidAnswer
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidAnswer
	}
	if len(object) != len(fields) {
		return nil, ErrInvalidAnswer
	}

	return object, nil
}

func answerArray(raw json.RawMessage, maximum int) ([]json.RawMessage, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || value[0] != '[' {
		return nil, ErrInvalidAnswer
	}

	var result []json.RawMessage
	if err := json.Unmarshal(value, &result); err != nil {
		return nil, ErrInvalidAnswer
	}
	if result == nil || len(result) > maximum {
		return nil, ErrInvalidAnswer
	}

	return result, nil
}

func answerString(raw json.RawMessage, maximum int) (string, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || value[0] != '"' {
		return "", ErrInvalidAnswer
	}

	var result string
	if err := json.Unmarshal(value, &result); err != nil {
		return "", ErrInvalidAnswer
	}
	if !utf8.ValidString(result) || strings.TrimSpace(result) == "" || utf8.RuneCountInString(result) > maximum {
		return "", ErrInvalidAnswer
	}

	return result, nil
}
