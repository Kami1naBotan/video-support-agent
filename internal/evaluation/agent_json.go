package evaluation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"strings"
	"unicode/utf8"
)

func EvidenceHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fieldError("evidence", "cannot encode content")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateArguments(values map[string]json.RawMessage) error {
	if values == nil || len(values) > MaxArgumentFields {
		return fieldError("arguments", "invalid object or field count")
	}
	total := 0
	for key, raw := range values {
		if err := validateKey("arguments", key); err != nil {
			return err
		}
		if len(raw) > MaxArgumentMatchBytes {
			return fieldError("arguments", "value is too large")
		}
		total += len(key) + len(raw)
		if total > MaxArgumentMatchBytes {
			return fieldError("arguments", "content is too large")
		}
		if _, err := argumentValue(raw); err != nil {
			return err
		}
	}
	return nil
}

func argumentValue(raw json.RawMessage) (any, error) {
	if len(raw) > MaxArgumentMatchBytes || !utf8.Valid(raw) {
		return nil, fieldError("arguments", "invalid encoding or size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fieldError("arguments", "trailing data")
	}
	return normalizeJSONNumbers(value), nil
}

func argumentsMatch(expected map[string]json.RawMessage, actual map[string]json.RawMessage) bool {
	for key, wanted := range expected {
		got, exists := actual[key]
		if !exists {
			return false
		}
		left, leftErr := argumentValue(wanted)
		right, rightErr := argumentValue(got)
		if leftErr != nil || rightErr != nil || !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

type canonicalNumber string

func normalizeJSONNumbers(value any) any {
	switch item := value.(type) {
	case json.Number:
		return normalizeNumber(item)
	case []any:
		for i := range item {
			item[i] = normalizeJSONNumbers(item[i])
		}
		return item
	case map[string]any:
		for key, child := range item {
			item[key] = normalizeJSONNumbers(child)
		}
		return item
	default:
		return value
	}
}

func normalizeNumber(number json.Number) canonicalNumber {
	text := string(number)
	exponent := new(big.Int)

	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent.SetString(text[index+1:], 10)
		text = text[:index]
	}

	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")

	fractionDigits := 0
	if index := strings.IndexByte(text, '.'); index >= 0 {
		fractionDigits = len(text) - index - 1
		text = text[:index] + text[index+1:]
	}

	text = strings.TrimLeft(text, "0")
	if text == "" {
		return canonicalNumber("0")
	}

	trimmed := strings.TrimRight(text, "0")
	trailingZeros := len(text) - len(trimmed)
	exponent.Add(exponent, big.NewInt(int64(trailingZeros-fractionDigits)))

	if negative {
		trimmed = "-" + trimmed
	}
	return canonicalNumber(trimmed + "e" + exponent.String())
}
