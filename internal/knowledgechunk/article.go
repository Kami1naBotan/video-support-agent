package knowledgechunk

import (
	"errors"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/model"
)

const Strategy = "article-v1"

var ErrInvalidArticle = errors.New("invalid article projection input")

func FromArticle(articleID uint64, content string) (model.KnowledgeChunk, error) {
	if articleID == 0 || !utf8.ValidString(content) {
		return model.KnowledgeChunk{}, ErrInvalidArticle
	}

	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return model.KnowledgeChunk{}, ErrInvalidArticle
	}

	return model.KnowledgeChunk{
		ArticleID:  articleID,
		ChunkIndex: 0,
		Content:    normalized,
	}, nil
}
