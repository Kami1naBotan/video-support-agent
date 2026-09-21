package handler

import "github.com/gin-gonic/gin"

type HealthHandler struct {
	serviceName string
}

func NewHealthHandler(serviceName string) *HealthHandler {
	return &HealthHandler{
		serviceName: serviceName,
	}
}

func (h *HealthHandler) Check(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "ok",
		"service": h.serviceName,
	})
}
