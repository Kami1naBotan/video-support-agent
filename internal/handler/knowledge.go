package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"video-support-agent/internal/middleware"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgeService interface {
	CreateDraft(ctx context.Context, input service.CreateKnowledgeDraftInput) (*model.RuleVersion, error)
	GetDraftByID(ctx context.Context, versionID uint64) (*service.KnowledgeDraftDetail, error)
}

type KnowledgeManagementService interface {
	GetDraftDetailByID(ctx context.Context, versionID uint64) (*service.KnowledgeDraftDetail, error)
	EditDraftWithVerification(ctx context.Context, input service.EditKnowledgeDraftInput) (*service.KnowledgeDraftEditResult, error)
	VerifyDraft(ctx context.Context, verifierID uint64, input service.VerifyKnowledgeDraftInput) (*model.RuleVerification, error)
}

type KnowledgeHandler struct {
	service KnowledgeService
}

func NewKnowledgeHandler(knowledgeService KnowledgeService) *KnowledgeHandler {
	return &KnowledgeHandler{service: knowledgeService}
}

func (h *KnowledgeHandler) Create(c *gin.Context) {
	request, err := decodeKnowledgeRequest[createKnowledgeDraftRequest](
		c.Request.Body,
	)
	if err != nil {
		writeBindError(c, err)
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

	input := service.CreateKnowledgeDraftInput{
		DocumentKey: request.DocumentKey,
		Version:     request.Version,
		CategoryID:  request.CategoryID,
		Title:       request.Title,
		Content:     request.Content,
		SourceURL:   request.SourceURL,
		EffectiveAt: request.EffectiveAt,
		ExpiresAt:   request.ExpiresAt,
		Articles:    articles,
	}

	version, err := h.service.CreateDraft(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}

	response := createKnowledgeDraftResponse{
		DocumentID: version.DocumentID,
		VersionID:  version.ID,
		Version:    version.Version,
		Status:     string(version.Status),
	}

	c.JSON(http.StatusCreated, response)
}

func (h *KnowledgeHandler) GetByID(c *gin.Context) {
	versionID, err := strconv.ParseUint(c.Param("version_id"), 10, 64)
	if err != nil || versionID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	management, ok := h.service.(KnowledgeManagementService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge management service is not configured"))
		return
	}

	detail, err := management.GetDraftDetailByID(c.Request.Context(), versionID)
	if err != nil {
		writeError(c, err)
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

	response := knowledgeDraftDetailResponse{
		DocumentID:        version.DocumentID,
		VersionID:         version.ID,
		Version:           version.Version,
		CategoryID:        version.CategoryID,
		RevisionType:      string(version.RevisionType),
		RevisionNote:      version.RevisionNote,
		RevisionSourceURL: version.RevisionSourceURL,
		Title:             version.Title,
		Content:           version.Content,
		SourceURL:         version.SourceURL,
		Status:            string(version.Status),
		EffectiveAt:       version.EffectiveAt,
		ExpiresAt:         version.ExpiresAt,
		Articles:          articles,
		ContentRevision:   version.ContentRevision,
		Verification:      buildKnowledgeVerificationResponse(version, detail.Verification),
	}

	c.JSON(http.StatusOK, response)
}

func decodeKnowledgeRequest[T any](body io.Reader) (*T, error) {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	var request *T
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}

	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, service.ErrInvalidArgument
	}

	if request == nil {
		return nil, service.ErrInvalidArgument
	}

	return request, nil
}

func buildKnowledgeVerificationResponse(version *model.RuleVersion, verification *model.RuleVerification) *knowledgeVerificationResponse {
	if verification == nil {
		return nil
	}

	return &knowledgeVerificationResponse{
		VerificationID:          verification.ID,
		VerifiedContentRevision: verification.VerifiedContentRevision,
		VerifiedBy:              verification.VerifiedBy,
		VerifiedAt:              verification.VerifiedAt,
		VerificationNote:        verification.VerificationNote,
		EvidenceURL:             verification.EvidenceURL,
		MatchesCurrentContent:   verification.VersionID == version.ID && verification.VerifiedContentRevision == version.ContentRevision,
	}
}

func (h *KnowledgeHandler) Edit(c *gin.Context) {
	versionID, err := strconv.ParseUint(c.Param("version_id"), 10, 64)
	if err != nil || versionID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	request, err := decodeKnowledgeRequest[editKnowledgeDraftRequest](
		c.Request.Body,
	)
	if err != nil {
		writeBindError(c, err)
		return
	}

	management, ok := h.service.(KnowledgeManagementService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge management service is not configured"))
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

	input := service.EditKnowledgeDraftInput{
		VersionID:               versionID,
		ExpectedContentRevision: request.ExpectedContentRevision,
		CategoryID:              request.CategoryID,
		Title:                   request.Title,
		Content:                 request.Content,
		SourceURL:               request.SourceURL,
		EffectiveAt:             request.EffectiveAt,
		ExpiresAt:               request.ExpiresAt,
		Articles:                articles,
	}

	result, err := management.EditDraftWithVerification(c.Request.Context(), input)
	if err != nil {
		writeError(c, err)
		return
	}

	response := editKnowledgeDraftResponse{
		VersionID:       result.Version.ID,
		ContentRevision: result.Version.ContentRevision,
		Verification:    buildKnowledgeVerificationResponse(result.Version, result.Verification),
	}

	c.JSON(http.StatusOK, response)
}

func (h *KnowledgeHandler) Verify(c *gin.Context) {
	verifierID, ok := middleware.CurrentUserID(c)
	if !ok {
		writeError(c, service.ErrUnauthorized)
		return
	}

	versionID, err := strconv.ParseUint(c.Param("version_id"), 10, 64)
	if err != nil || versionID == 0 {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	request, err := decodeKnowledgeRequest[verifyKnowledgeDraftRequest](
		c.Request.Body,
	)
	if err != nil {
		writeBindError(c, err)
		return
	}

	management, ok := h.service.(KnowledgeManagementService)
	if !ok {
		writeError(c, fmt.Errorf("knowledge management service is not configured"))
		return
	}

	input := service.VerifyKnowledgeDraftInput{
		VersionID:               versionID,
		ExpectedContentRevision: request.ExpectedContentRevision,
		VerificationNote:        request.VerificationNote,
		EvidenceURL:             request.EvidenceURL,
	}

	verification, err := management.VerifyDraft(c.Request.Context(), verifierID, input)
	if err != nil {
		writeError(c, err)
		return
	}

	response := verifyKnowledgeDraftResponse{
		VerificationID:          verification.ID,
		VersionID:               verification.VersionID,
		VerifiedContentRevision: verification.VerifiedContentRevision,
		VerifiedBy:              verification.VerifiedBy,
		VerifiedAt:              verification.VerifiedAt,
	}

	c.JSON(http.StatusCreated, response)
}
