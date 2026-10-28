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

type KnowledgeWithdrawalService interface {
	WithdrawVersion(ctx context.Context, operatorID uint64, input service.WithdrawKnowledgeVersionInput) (*service.KnowledgeWithdrawalResult, error)
}

func (h *KnowledgeHandler) Withdraw(c *gin.Context) {
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

	request, err := decodeKnowledgeRequest[withdrawKnowledgeVersionRequest](c.Request.Body)
	if err != nil {
		writeBindError(c, err)
		return
	}
	if request.VersionID == 0 || request.ExpectedPublicationRevision == nil {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	withdrawal, ok := h.service.(KnowledgeWithdrawalService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge withdrawal service is not configured"))
		return
	}

	input := service.WithdrawKnowledgeVersionInput{
		DocumentID:                  documentID,
		VersionID:                   request.VersionID,
		ExpectedPublicationRevision: *request.ExpectedPublicationRevision,
	}

	result, err := withdrawal.WithdrawVersion(c.Request.Context(), operatorID, input)
	if err != nil {
		writeError(c, err)
		return
	}
	if result == nil {
		writeError(c, fmt.Errorf("knowledge withdrawal service returned an empty result"))
		return
	}

	c.JSON(http.StatusOK, withdrawKnowledgeVersionResponse{
		DocumentID:          result.DocumentID,
		VersionID:           result.VersionID,
		PublicationRevision: result.PublicationRevision,
		WithdrawnAt:         result.WithdrawnAt,
	})
}
