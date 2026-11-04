package tools

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/service"
)

func decodeObject(raw string, dst any, allowed ...string) error {
	bad := service.ErrInvalidArgument
	if len(raw) == 0 || len(raw) > MaxArgumentBytes || !utf8.ValidString(raw) {
		return bad
	}

	known := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		known[name] = true
	}
	seen := make(map[string]bool, len(allowed))

	dec := json.NewDecoder(strings.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return bad
	}

	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return bad
		}
		key, ok := token.(string)
		if !ok || !known[key] || seen[key] {
			return bad
		}
		seen[key] = true

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return bad
		}
		value = bytes.TrimSpace(value)
		if len(value) == 0 || bytes.Equal(value, []byte("null")) || value[0] == '{' || value[0] == '[' {
			return bad
		}
	}

	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return bad
	}

	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return bad
	}

	typed := json.NewDecoder(strings.NewReader(raw))
	typed.DisallowUnknownFields()
	if err := typed.Decode(dst); err != nil {
		return bad
	}
	return nil
}

func parseID(raw string) (uint64, error) {
	if len(raw) == 0 || len(raw) > 20 || raw[0] < '1' || raw[0] > '9' {
		return 0, service.ErrInvalidArgument
	}

	for i := 1; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, service.ErrInvalidArgument
		}
	}

	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, service.ErrInvalidArgument
	}
	return id, nil
}
