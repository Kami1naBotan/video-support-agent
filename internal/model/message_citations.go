package model

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const MaxMessageCitationsJSONBytes = 128 << 10

var ErrInvalidMessageCitations = errors.New("invalid message citations")

type MessageCitations []RuleCitation

var (
	_ sql.Scanner   = (*MessageCitations)(nil)
	_ driver.Valuer = MessageCitations(nil)
)

func NewMessageCitations(items []RuleCitation) MessageCitations {
	return MessageCitations(items).Clone()
}

func (c MessageCitations) Clone() MessageCitations {
	result := make(MessageCitations, len(c))
	for i, citation := range c {
		result[i] = citation.Clone()
	}
	return result
}

func (MessageCitations) GormDataType() string {
	return "json"
}

func (c MessageCitations) Value() (driver.Value, error) {
	if len(c) == 0 {
		return nil, nil
	}

	data, err := c.MarshalJSON()
	if err != nil {
		return nil, err
	}

	return string(data), nil
}

func (c *MessageCitations) Scan(value any) error {
	if c == nil {
		return ErrInvalidMessageCitations
	}

	*c = nil

	switch value := value.(type) {
	case nil:
		return nil

	case []byte:
		return c.UnmarshalJSON(value)

	case string:
		if len(value) > MaxMessageCitationsJSONBytes {
			return ErrInvalidMessageCitations
		}
		return c.UnmarshalJSON([]byte(value))

	default:
		return ErrInvalidMessageCitations
	}
}

func (c MessageCitations) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("[]"), nil
	}

	for _, citation := range c {
		if !validMessageCitationText(citation) {
			return nil, ErrInvalidMessageCitations
		}
	}

	data, err := json.Marshal([]RuleCitation(c))
	if err != nil || len(data) > MaxMessageCitationsJSONBytes {
		return nil, ErrInvalidMessageCitations
	}

	return data, nil
}

func (c *MessageCitations) UnmarshalJSON(data []byte) error {
	if c == nil {
		return ErrInvalidMessageCitations
	}

	*c = nil

	if len(data) > MaxMessageCitationsJSONBytes || !utf8.Valid(data) {
		return ErrInvalidMessageCitations
	}

	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		return nil
	}
	if len(data) == 0 || data[0] != '[' {
		return ErrInvalidMessageCitations
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var decoded []*RuleCitation
	if err := decoder.Decode(&decoded); err != nil {
		return ErrInvalidMessageCitations
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrInvalidMessageCitations
	}

	result := make(MessageCitations, len(decoded))
	for i, citation := range decoded {
		if citation == nil || !validMessageCitationText(*citation) {
			return ErrInvalidMessageCitations
		}
		result[i] = *citation
	}

	*c = result
	return nil
}

func validMessageCitationText(citation RuleCitation) bool {
	texts := []string{
		citation.RefID,
		citation.DocumentID,
		citation.VersionID,
		citation.ArticleID,
		citation.CategoryID,
		citation.ChunkID,
		citation.Version,
		citation.Title,
		citation.SectionPath,
		citation.ArticleNumber,
		citation.SourceURL,
	}

	for _, text := range texts {
		if !utf8.ValidString(text) {
			return false
		}
	}

	for _, chunkID := range citation.ChunkIDs {
		if !utf8.ValidString(chunkID) {
			return false
		}
	}

	return true
}
