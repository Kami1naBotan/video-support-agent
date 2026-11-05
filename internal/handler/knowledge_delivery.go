package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type KnowledgeDeliveryService interface {
	GetDocumentDeliveries(context.Context, uint64, uint64, int) (*service.KnowledgeDeliveryPage, error)
}

func deliveryPositiveUint(raw string) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != raw {
		return 0, service.ErrInvalidArgument
	}
	return value, nil
}

func (h *KnowledgeHandler) GetDocumentDeliveries(c *gin.Context) {
	documentID, err := deliveryPositiveUint(c.Param("document_id"))
	if err != nil {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeError(c, service.ErrInvalidArgument)
		return
	}

	for key, values := range query {
		if (key != "limit" && key != "before") || len(values) != 1 {
			writeError(c, service.ErrInvalidArgument)
			return
		}
	}

	limit := 20
	if values, exists := query["limit"]; exists {
		value, err := deliveryPositiveUint(values[0])
		if err != nil || value > 100 {
			writeError(c, service.ErrInvalidArgument)
			return
		}
		limit = int(value)
	}

	var before uint64
	if values, exists := query["before"]; exists {
		before, err = deliveryPositiveUint(values[0])
		if err != nil {
			writeError(c, service.ErrInvalidArgument)
			return
		}
	}

	deliveries, ok := h.service.(KnowledgeDeliveryService)
	if !ok {
		writeError(c, errors.New("knowledge delivery service is not configured"))
		return
	}

	page, err := deliveries.GetDocumentDeliveries(c.Request.Context(), documentID, before, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	if page == nil {
		writeError(c, errors.New("knowledge delivery service returned no page"))
		return
	}

	c.JSON(http.StatusOK, page)
}
