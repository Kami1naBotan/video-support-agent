package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"video-support-agent/internal/middleware"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgePublicationService interface {
	PublishVersion(ctx context.Context, operatorID uint64, input service.PublishKnowledgeVersionInput) (*service.KnowledgePublicationResult, error)
	GetCurrentByDocumentID(ctx context.Context, documentID uint64) (*service.KnowledgeCurrentDetail, error)
	GetDocumentState(ctx context.Context, documentID uint64) (*service.KnowledgeDocumentState, error)
}

func (h *KnowledgeHandler) Publish(c *gin.Context) {
	operatorID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	documentID, err := strconv.ParseUint(c.Param("document_id"), 10, 64)
	if err != nil || documentID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	request, err := decodeKnowledgeRequest[publishKnowledgeVersionRequest](c.Request.Body)
	if err != nil {
		writeBindError(c, err)
		return
	}

	if request.VersionID == 0 || request.ExpectedPublicationRevision == nil {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	publication, ok := h.service.(KnowledgePublicationService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge publication service is not configured"))
		return
	}

	input := service.PublishKnowledgeVersionInput{
		DocumentID:                  documentID,
		VersionID:                   request.VersionID,
		ExpectedPublicationRevision: *request.ExpectedPublicationRevision,
	}

	result, err := publication.PublishVersion(c.Request.Context(), operatorID, input)
	if err != nil {
		writeError(c, err)
		return
	}
	if result == nil {
		writeError(c, fmt.Errorf("knowledge publication service returned an empty result"))
		return
	}

	c.JSON(http.StatusOK, publishKnowledgeVersionResponse{
		DocumentID:          result.DocumentID,
		VersionID:           result.VersionID,
		PublicationRevision: result.PublicationRevision,
		PublishedAt:         result.PublishedAt,
	})
}

func (h *KnowledgeHandler) GetCurrent(c *gin.Context) {
	documentID, err := strconv.ParseUint(c.Param("document_id"), 10, 64)
	if err != nil || documentID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	publication, ok := h.service.(KnowledgePublicationService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge publication service is not configured"))
		return
	}

	detail, err := publication.GetCurrentByDocumentID(c.Request.Context(), documentID)
	if err != nil {
		writeError(c, err)
		return
	}
	if detail == nil || detail.Version == nil {
		writeError(c, fmt.Errorf("current knowledge service returned an empty result"))
		return
	}

	articles := make([]ruleArticleResponse, len(detail.Articles))
	for i, article := range detail.Articles {
		articles[i] = ruleArticleResponse{
			ArticleID:     article.ID,
			ArticleOrder:  article.ArticleOrder,
			SectionPath:   article.SectionPath,
			ArticleNumber: article.ArticleNumber,
			Content:       article.Content,
		}
	}

	version := detail.Version
	c.JSON(http.StatusOK, knowledgeCurrentDetailResponse{
		DocumentID:  version.DocumentID,
		VersionID:   version.ID,
		Version:     version.Version,
		CategoryID:  version.CategoryID,
		Title:       version.Title,
		Content:     version.Content,
		SourceURL:   version.SourceURL,
		EffectiveAt: version.EffectiveAt,
		ExpiresAt:   version.ExpiresAt,
		Articles:    articles,
	})
}

func (h *KnowledgeHandler) GetDocumentState(c *gin.Context) {
	documentID, err := strconv.ParseUint(c.Param("document_id"), 10, 64)
	if err != nil || documentID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	publication, ok := h.service.(KnowledgePublicationService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge publication service is not configured"))
		return
	}

	state, err := publication.GetDocumentState(c.Request.Context(), documentID)
	if err != nil {
		writeError(c, err)
		return
	}
	if state == nil {
		writeError(c, fmt.Errorf("knowledge document service returned an empty result"))
		return
	}

	c.JSON(http.StatusOK, knowledgeDocumentStateResponse{
		DocumentID:          state.DocumentID,
		CurrentVersionID:    state.CurrentVersionID,
		PublicationRevision: state.PublicationRevision,
	})
}
