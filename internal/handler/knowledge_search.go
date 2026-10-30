package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"video-support-agent/internal/middleware"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgeSearchHandler struct {
	retriever retrieval.Retriever
}

func NewKnowledgeSearchHandler(retriever retrieval.Retriever) *KnowledgeSearchHandler {
	return &KnowledgeSearchHandler{
		retriever: retriever,
	}
}

func (h *KnowledgeSearchHandler) Search(c *gin.Context) {
	if _, ok := middleware.CurrentUserID(c); !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}
	if err := c.Request.Context().Err(); err != nil {
		writeError(c, err)
		return
	}

	query, opts, err := parseKnowledgeSearchQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeError(c, err)
		return
	}

	if h == nil || h.retriever == nil {
		writeError(c, errors.New("knowledge search is not configured"))
		return
	}

	chunks, err := h.retriever.Retrieve(c.Request.Context(), query, opts)
	if contextErr := c.Request.Context().Err(); contextErr != nil {
		writeError(c, contextErr)
		return
	}
	if err != nil {
		writeError(c, err)
		return
	}
	if chunks == nil || len(chunks) > opts.Limit {
		writeError(c, retrieval.ErrInvalidCandidates)
		return
	}

	items := make([]knowledgeSearchItem, 0, len(chunks))
	for _, chunk := range chunks {
		items = append(items, toKnowledgeSearchItem(chunk))
	}

	if err := c.Request.Context().Err(); err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, knowledgeSearchResponse{
		Items: items,
		Count: len(items),
		Limit: opts.Limit,
	})
}

func parseKnowledgeSearchQuery(rawQuery string) (string, retrieval.SearchOptions, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
	}

	for name, entries := range values {
		switch name {
		case "q", "document_id", "category_id", "article_number", "section", "limit", "highlight":
		default:
			return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
		}
		if len(entries) != 1 {
			return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
		}
	}

	documentID, err := parseKnowledgeSearchUint(values, "document_id")
	if err != nil {
		return "", retrieval.SearchOptions{}, err
	}
	categoryID, err := parseKnowledgeSearchUint(values, "category_id")
	if err != nil {
		return "", retrieval.SearchOptions{}, err
	}
	limit, err := parseKnowledgeSearchUint(values, "limit")
	if err != nil || limit > uint64(retrieval.MaxLimit) {
		return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
	}

	highlight := false
	if entries, exists := values["highlight"]; exists {
		switch entries[0] {
		case "true":
			highlight = true
		case "false":
			highlight = false
		default:
			return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
		}
	}

	opts := retrieval.SearchOptions{
		DocumentID:    documentID,
		CategoryID:    categoryID,
		ArticleNumber: values.Get("article_number"),
		Section:       values.Get("section"),
		Limit:         int(limit),
		Highlight:     highlight,
	}

	return retrieval.NormalizeSearch(values.Get("q"), opts)
}

func parseKnowledgeSearchUint(values url.Values, name string) (uint64, error) {
	entries, exists := values[name]
	if !exists {
		return 0, nil
	}
	if len(entries) != 1 {
		return 0, retrieval.ErrInvalidQuery
	}

	raw := entries[0]
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != raw {
		return 0, retrieval.ErrInvalidQuery
	}

	return value, nil
}
