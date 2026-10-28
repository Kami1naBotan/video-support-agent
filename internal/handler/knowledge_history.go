package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgeHistoryService interface {
	GetHistoryByVersionID(ctx context.Context, versionID uint64) (*service.KnowledgeHistoryDetail, error)
	GetHistoryArticle(ctx context.Context, versionID uint64, articleID uint64) (*service.KnowledgeHistoryArticleDetail, error)
}

type knowledgeHistoryVersionResponse struct {
	DocumentID   uint64     `json:"document_id"`
	VersionID    uint64     `json:"version_id"`
	Version      string     `json:"version"`
	CategoryID   uint64     `json:"category_id"`
	RevisionType string     `json:"revision_type"`
	Title        string     `json:"title"`
	SourceURL    string     `json:"source_url"`
	EffectiveAt  *time.Time `json:"effective_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	PublishedAt  *time.Time `json:"published_at"`
}

type knowledgeHistoryStateResponse struct {
	IsCurrent          bool `json:"is_current"`
	IsSuperseded       bool `json:"is_superseded"`
	IsExpired          bool `json:"is_expired"`
	IsWithdrawn        bool `json:"is_withdrawn"`
	IsCurrentEffective bool `json:"is_current_effective"`
}

type knowledgeHistoryResponse struct {
	Version  knowledgeHistoryVersionResponse `json:"version"`
	Content  string                          `json:"content"`
	Articles []ruleArticleResponse           `json:"articles"`
	State    knowledgeHistoryStateResponse   `json:"state"`
}

type knowledgeHistoryArticleResponse struct {
	Version knowledgeHistoryVersionResponse `json:"version"`
	Article ruleArticleResponse             `json:"article"`
	State   knowledgeHistoryStateResponse   `json:"state"`
}

func (h *KnowledgeHandler) GetHistory(c *gin.Context) {
	versionID, err := strconv.ParseUint(c.Param("version_id"), 10, 64)
	if err != nil || versionID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	history, ok := h.service.(KnowledgeHistoryService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge history service is not configured"))
		return
	}

	detail, err := history.GetHistoryByVersionID(c.Request.Context(), versionID)
	if err != nil {
		writeError(c, err)
		return
	}
	if detail == nil || detail.Version == nil {
		writeError(c, fmt.Errorf("knowledge history returned an empty result"))
		return
	}

	articles := make([]ruleArticleResponse, len(detail.Articles))
	for i, article := range detail.Articles {
		articles[i] = buildKnowledgeHistoryArticleResponse(article)
	}

	c.JSON(http.StatusOK, knowledgeHistoryResponse{
		Version:  buildKnowledgeHistoryVersionResponse(detail.Version),
		Content:  detail.Version.Content,
		Articles: articles,
		State:    buildKnowledgeHistoryStateResponse(detail.State),
	})
}

func (h *KnowledgeHandler) GetHistoryArticle(c *gin.Context) {
	versionID, err := strconv.ParseUint(c.Param("version_id"), 10, 64)
	if err != nil || versionID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	articleID, err := strconv.ParseUint(c.Param("article_id"), 10, 64)
	if err != nil || articleID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	history, ok := h.service.(KnowledgeHistoryService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge history service is not configured"))
		return
	}

	detail, err := history.GetHistoryArticle(c.Request.Context(), versionID, articleID)
	if err != nil {
		writeError(c, err)
		return
	}
	if detail == nil || detail.Version == nil || detail.Article == nil {
		writeError(c, fmt.Errorf("knowledge history article returned an empty result"))
		return
	}

	c.JSON(http.StatusOK, knowledgeHistoryArticleResponse{
		Version: buildKnowledgeHistoryVersionResponse(detail.Version),
		Article: buildKnowledgeHistoryArticleResponse(*detail.Article),
		State:   buildKnowledgeHistoryStateResponse(detail.State),
	})
}

func buildKnowledgeHistoryVersionResponse(version *model.RuleVersion) knowledgeHistoryVersionResponse {
	return knowledgeHistoryVersionResponse{
		DocumentID:   version.DocumentID,
		VersionID:    version.ID,
		Version:      version.Version,
		CategoryID:   version.CategoryID,
		RevisionType: string(version.RevisionType),
		Title:        version.Title,
		SourceURL:    version.SourceURL,
		EffectiveAt:  version.EffectiveAt,
		ExpiresAt:    version.ExpiresAt,
		PublishedAt:  version.PublishedAt,
	}
}

func buildKnowledgeHistoryArticleResponse(article model.RuleArticle) ruleArticleResponse {
	return ruleArticleResponse{
		ArticleID:     article.ID,
		ArticleOrder:  article.ArticleOrder,
		SectionPath:   article.SectionPath,
		ArticleNumber: article.ArticleNumber,
		Content:       article.Content,
	}
}

func buildKnowledgeHistoryStateResponse(state service.KnowledgeHistoryState) knowledgeHistoryStateResponse {
	return knowledgeHistoryStateResponse{
		IsCurrent:          state.IsCurrent,
		IsSuperseded:       state.IsSuperseded,
		IsExpired:          state.IsExpired,
		IsWithdrawn:        state.IsWithdrawn,
		IsCurrentEffective: state.IsCurrentEffective,
	}
}
