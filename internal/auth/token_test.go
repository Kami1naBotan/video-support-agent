package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 仅供测试使用，不能作为实际部署密钥。
const tokenTestSecret = "0123456789abcdef0123456789abcdef"

func TestTokenManagerIssueAndParse(t *testing.T) {
	manager, err := NewTokenManager(
		tokenTestSecret,
		"bili-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := manager.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	userID, err := manager.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if userID != 7 {
		t.Fatalf("expected user ID 7, got %d", userID)
	}
}

func TestTokenManagerRejectsInvalidTokens(t *testing.T) {
	manager, err := NewTokenManager(
		tokenTestSecret,
		"bili-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		change func(*jwt.RegisteredClaims)
		secret string
		method jwt.SigningMethod
	}{
		{
			name:   "wrong signing key",
			secret: strings.Repeat("x", 32),
		},
		{
			name: "wrong issuer",
			change: func(c *jwt.RegisteredClaims) {
				c.Issuer = "another-service"
			},
		},
		{
			name: "expired",
			change: func(c *jwt.RegisteredClaims) {
				c.IssuedAt = jwt.NewNumericDate(
					time.Now().Add(-2 * time.Hour),
				)
				c.ExpiresAt = jwt.NewNumericDate(
					time.Now().Add(-time.Hour),
				)
			},
		},
		{
			name: "missing expiration",
			change: func(c *jwt.RegisteredClaims) {
				c.ExpiresAt = nil
			},
		},
		{
			name: "invalid user ID",
			change: func(c *jwt.RegisteredClaims) {
				c.Subject = "not-a-number"
			},
		},
		{
			name: "zero user ID",
			change: func(c *jwt.RegisteredClaims) {
				c.Subject = "0"
			},
		},
		{
			name:   "unexpected algorithm",
			method: jwt.SigningMethodHS384,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			claims := jwt.RegisteredClaims{
				Issuer:    "bili-support",
				Subject:   "7",
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			}

			if tt.change != nil {
				tt.change(&claims)
			}

			secret := tt.secret
			if secret == "" {
				secret = tokenTestSecret
			}

			method := tt.method
			if method == nil {
				method = jwt.SigningMethodHS256
			}

			raw, err := jwt.NewWithClaims(
				method,
				claims,
			).SignedString([]byte(secret))
			if err != nil {
				t.Fatal(err)
			}

			userID, err := manager.Parse(raw)
			if err == nil {
				t.Fatal("expected token to be rejected")
			}
			if userID != 0 {
				t.Fatal("invalid token must not return a user ID")
			}
		})
	}
}

func TestTokenManagerRejectsMalformedToken(t *testing.T) {
	manager, err := NewTokenManager(
		tokenTestSecret,
		"bili-support",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Parse("not-a-jwt"); err == nil {
		t.Fatal("expected malformed token to be rejected")
	}
}

func TestNewTokenManagerRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		issuer string
		ttl    time.Duration
	}{
		{"short secret", "short", "bili-support", time.Hour},
		{"empty issuer", tokenTestSecret, "", time.Hour},
		{"invalid TTL", tokenTestSecret, "bili-support", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewTokenManager(tt.secret, tt.issuer, tt.ttl)
			if err == nil {
				t.Fatal("expected invalid configuration to be rejected")
			}
		})
	}
}
