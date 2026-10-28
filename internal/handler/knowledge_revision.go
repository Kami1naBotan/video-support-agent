package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgeRevisionService interface {
	CreateRevision(ctx context.Context, input service.CreateKnowledgeRevisionInput) (*model.RuleVersion, error)
}

func (h *KnowledgeHandler) CreateRevision(c *gin.Context) {
	documentID, err := strconv.ParseUint(c.Param("document_id"), 10, 64)
	if err != nil || documentID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	request, err := decodeKnowledgeRequest[createKnowledgeRevisionRequest](c.Request.Body)
	if err != nil {
		writeBindError(c, err)
		return
	}

	revisions, ok := h.service.(KnowledgeRevisionService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge revision service is not configured"))
		return
	}

	articles := make([]service.CreateRuleArticleInput, len(request.Articles))
	for i, article := range request.Articles {
		articles[i] = service.CreateRuleArticleInput{
			SectionPath:   article.SectionPath,
			ArticleNumber: article.ArticleNumber,
			Content:       article.Content,
		}
	}

	input := service.CreateKnowledgeRevisionInput{
		DocumentID:        documentID,
		Version:           request.Version,
		RevisionType:      model.RuleRevisionType(request.RevisionType),
		RevisionNote:      request.RevisionNote,
		RevisionSourceURL: request.RevisionSourceURL,
		CategoryID:        request.CategoryID,
		Title:             request.Title,
		Content:           request.Content,
		SourceURL:         request.SourceURL,
		EffectiveAt:       request.EffectiveAt,
		ExpiresAt:         request.ExpiresAt,
		Articles:          articles,
	}

	version, err := revisions.CreateRevision(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}
	if version == nil {
		writeError(c, fmt.Errorf("knowledge revision service returned an empty result"))
		return
	}

	c.JSON(http.StatusCreated, createKnowledgeDraftResponse{
		DocumentID: version.DocumentID,
		VersionID:  version.ID,
		Version:    version.Version,
		Status:     string(version.Status),
	})
}
