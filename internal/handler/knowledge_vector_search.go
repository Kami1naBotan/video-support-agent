package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"video-support-agent/internal/generationquery"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

const maxVectorRawQueryBytes = 8 << 10

type KnowledgeVectorSearchHandler struct {
	retriever retrieval.HydratedRetriever
}

func NewKnowledgeVectorSearchHandler(
	retriever retrieval.HydratedRetriever,
) *KnowledgeVectorSearchHandler {
	return &KnowledgeVectorSearchHandler{retriever: retriever}
}

type knowledgeVectorSearchItem struct {
	knowledgeSearchItem
	ArticleContent string `json:"article_content"`
}

type knowledgeVectorSearchResponse struct {
	Items []knowledgeVectorSearchItem `json:"items"`
	Count int                         `json:"count"`
	Limit int                         `json:"limit"`
}

func (h *KnowledgeVectorSearchHandler) Search(c *gin.Context) {
	if _, ok := middleware.CurrentUserID(c); !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}
	if err := c.Request.Context().Err(); err != nil {
		writeVectorSearchError(c, err)
		return
	}

	query, opts, err := parseKnowledgeVectorSearchQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeVectorSearchError(c, err)
		return
	}
	if h == nil || h.retriever == nil {
		writeVectorSearchError(c, errors.New("vector search is not configured"))
		return
	}

	results, err := h.retriever.Retrieve(c.Request.Context(), query, opts)
	if contextErr := c.Request.Context().Err(); contextErr != nil {
		writeVectorSearchError(c, contextErr)
		return
	}
	if err != nil {
		writeVectorSearchError(c, err)
		return
	}
	if results == nil || len(results) > opts.Limit {
		writeVectorSearchError(c, retrieval.ErrInvalidCandidates)
		return
	}

	items := make([]knowledgeVectorSearchItem, 0, len(results))
	for _, result := range results {
		if err := c.Request.Context().Err(); err != nil {
			writeVectorSearchError(c, err)
			return
		}

		chunk := toKnowledgeSearchItem(result.Chunk)
		chunk.Highlights = make(map[string][]string)

		items = append(items, knowledgeVectorSearchItem{
			knowledgeSearchItem: chunk,
			ArticleContent:      result.ArticleContent,
		})
	}

	body, err := json.Marshal(knowledgeVectorSearchResponse{
		Items: items,
		Count: len(items),
		Limit: opts.Limit,
	})
	if err != nil {
		writeVectorSearchError(c, retrieval.ErrInvalidCandidates)
		return
	}
	if err := c.Request.Context().Err(); err != nil {
		writeVectorSearchError(c, err)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func parseKnowledgeVectorSearchQuery(rawQuery string) (string, retrieval.SearchOptions, error) {
	if len(rawQuery) > maxVectorRawQueryBytes {
		return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", retrieval.SearchOptions{}, retrieval.ErrInvalidQuery
	}

	for name, entries := range values {
		switch name {
		case "q", "document_id", "category_id", "limit":
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

	return retrieval.NormalizeSearch(values.Get("q"), retrieval.SearchOptions{
		DocumentID: documentID,
		CategoryID: categoryID,
		Limit:      int(limit),
	})
}

func writeVectorSearchError(c *gin.Context, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(c, err)
		return
	}

	status := http.StatusInternalServerError
	response := ErrorResponse{
		Code:    "internal_error",
		Message: "internal server error",
	}

	var milvusAPIError *milvus.APIError
	switch {
	case errors.Is(err, retrieval.ErrInvalidQuery):
		status = http.StatusBadRequest
		response = ErrorResponse{
			Code: "invalid_search_query", Message: "invalid search query",
		}
	case errors.Is(err, embedding.ErrInputTooLarge), errors.Is(err, embedding.ErrContextLength):
		status = http.StatusBadRequest
		response = ErrorResponse{
			Code:    "vector_query_too_large",
			Message: "query exceeds the embedding input limit",
		}
	case errors.Is(err, retrieval.ErrHydrationLimit):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "search_hydration_limit",
			Message: "rule data exceeds the retrieval read budget",
		}
	case errors.Is(err, retrieval.ErrHydrationFailed):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "search_hydration_failed",
			Message: "rule hydration is temporarily unavailable",
		}
	case errors.Is(err, retrieval.ErrIndexNotReady):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "search_index_not_ready",
			Message: "search index is not ready",
		}
	case errors.Is(err, generationquery.ErrResolve):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "search_generation_unavailable",
			Message: "published search generation is temporarily unavailable",
		}
	case errors.Is(err, generationquery.ErrTarget):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "search_generation_invalid",
			Message: "published search generation is invalid",
		}
	case errors.Is(err, embedding.ErrModelMismatch):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "vector_model_mismatch",
			Message: "embedding model does not match the required contract",
		}
	case errors.As(err, &milvusAPIError) && milvusAPIError.Code == 1807:
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "vector_backend_rate_limited",
			Message: "vector backend is temporarily rate limited",
		}
	case errors.Is(err, embedding.ErrUnavailable), errors.Is(err, milvus.ErrUnavailable):
		status = http.StatusServiceUnavailable
		response = ErrorResponse{
			Code:    "vector_search_unavailable",
			Message: "vector search dependency is temporarily unavailable",
		}
	case errors.Is(err, embedding.ErrResponse), errors.Is(err, milvus.ErrResponse):
		status = http.StatusBadGateway
		response = ErrorResponse{
			Code:    "invalid_vector_backend_response",
			Message: "vector search dependency returned an invalid response",
		}
	case errors.Is(err, embedding.ErrRejected), errors.Is(err, milvus.ErrRejected):
		status = http.StatusBadGateway
		response = ErrorResponse{
			Code:    "vector_backend_rejected",
			Message: "vector search dependency rejected the request",
		}
	}

	c.JSON(status, response)
}
