package handler

import (
	"net/http"
	"video-support-agent/internal/auth"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type AuthHandler struct {
	service *service.AuthService
	tokens  *auth.TokenManager
}

func NewAuthHandler(authService *service.AuthService, tokens *auth.TokenManager) *AuthHandler {
	return &AuthHandler{
		service: authService,
		tokens:  tokens,
	}
}

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *AuthHandler) Register(c *gin.Context) {
	var request registerRequest

	if err := c.ShouldBindJSON(&request); err != nil {
		writeBindError(c, err)
		return
	}

	user, err := h.service.Register(
		c.Request.Context(),
		request.Username,
		request.Password,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	c.Header("Cache-Control", "no-store")

	var request loginRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		writeBindError(c, err)
		return
	}

	user, err := h.service.Authenticate(
		c.Request.Context(),
		request.Username,
		request.Password,
	)
	if err != nil {
		writeError(c, err)
		return
	}

	token, err := h.tokens.Issue(user.ID)
	if err != nil {
		writeError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token": token,
		"token_type":   "Bearer",
	})
}
