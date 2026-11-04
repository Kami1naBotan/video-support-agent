package confirmation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/service"
)

const (
	appealTokenPrefix   = "ac1"
	appealTokenMaxBytes = 8192
	appealMACDomain     = "video-support-agent/appeal-confirmation/ac1\n"
)

type AppealSigner struct {
	key      []byte
	issuer   string
	audience string
	clock    func() time.Time
}

var _ service.AppealConfirmations = (*AppealSigner)(nil)

func NewAppealSigner(cfg config.AppealConfirmationConfig) (*AppealSigner, error) {
	return NewAppealSignerWithClock(cfg, time.Now)
}

func NewAppealSignerWithClock(cfg config.AppealConfirmationConfig, clock func() time.Time) (*AppealSigner, error) {
	if len(cfg.KeyBase64) != 44 || !validScope(cfg.Issuer) || !validScope(cfg.Audience) || clock == nil {
		return nil, service.ErrAppealConfiguration
	}

	key, err := base64.StdEncoding.Strict().DecodeString(cfg.KeyBase64)
	if err != nil || len(key) != 32 || base64.StdEncoding.EncodeToString(key) != cfg.KeyBase64 {
		return nil, service.ErrAppealConfiguration
	}

	return &AppealSigner{
		key:      append([]byte(nil), key...),
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		clock:    clock,
	}, nil
}

func validScope(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := range value {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

type appealClaims struct {
	Version        uint8  `json:"v"`
	Purpose        string `json:"purpose"`
	Issuer         string `json:"iss"`
	Audience       string `json:"aud"`
	UserID         uint64 `json:"user_id,string"`
	ConversationID uint64 `json:"conversation_id,string"`
	IdempotencyKey string `json:"key"`
	RequestHash    string `json:"request_hash"`
	PolicyDigest   string `json:"policy_digest"`
	IssuedAtMS     int64  `json:"iat_ms"`
	ExpiresAtMS    int64  `json:"exp_ms"`
}

func (s *AppealSigner) ready() bool {
	return s != nil && len(s.key) == 32 && s.clock != nil && validScope(s.issuer) && validScope(s.audience)
}

func validInstant(at time.Time) bool {
	year := at.UTC().Year()
	return !at.IsZero() && year >= 1000 && year <= 9999 && at.Equal(at.Truncate(time.Millisecond))
}

func (s *AppealSigner) now() (time.Time, error) {
	at := s.clock().UTC().Truncate(time.Millisecond)
	if !validInstant(at) {
		return time.Time{}, service.ErrAppealConfiguration
	}
	return at, nil
}

func (s *AppealSigner) Issue(ctx context.Context, binding service.AppealConfirmationBinding, issuedAt, expiresAt time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !s.ready() {
		return "", service.ErrAppealConfiguration
	}
	if err := service.ValidateAppealConfirmationBinding(binding); err != nil {
		return "", err
	}

	if !validInstant(issuedAt) || !validInstant(expiresAt) || !expiresAt.Equal(issuedAt.Add(service.AppealConfirmationLifetime)) {
		return "", service.ErrInvalidArgument
	}

	now, err := s.now()
	if err != nil {
		return "", err
	}
	if issuedAt.After(now) || !now.Before(expiresAt) {
		return "", service.ErrInvalidArgument
	}

	claims := appealClaims{
		Version:        1,
		Purpose:        service.AppealConfirmationPurpose,
		Issuer:         s.issuer,
		Audience:       s.audience,
		UserID:         binding.UserID,
		ConversationID: binding.ConversationID,
		IdempotencyKey: binding.IdempotencyKey,
		RequestHash:    binding.RequestHash,
		PolicyDigest:   binding.PolicyDigest,
		IssuedAtMS:     issuedAt.UnixMilli(),
		ExpiresAtMS:    expiresAt.UnixMilli(),
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", service.ErrAppealConfiguration
	}

	unsigned := appealTokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload)

	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(s.mac(unsigned))

	if len(token) > appealTokenMaxBytes {
		return "", service.ErrAppealConfiguration
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *AppealSigner) mac(unsigned string) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(appealMACDomain))
	_, _ = mac.Write([]byte(unsigned))
	return mac.Sum(nil)
}

func decodeSegment(raw string) ([]byte, bool) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(data) != raw {
		return nil, false
	}
	return data, true
}

func (s *AppealSigner) Verify(ctx context.Context, token string, expected service.AppealConfirmationBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.ready() {
		return service.ErrAppealConfiguration
	}
	if err := service.ValidateAppealConfirmationBinding(expected); err != nil {
		return err
	}

	rejected := service.ErrAppealConfirmationRequired

	if len(token) == 0 || len(token) > appealTokenMaxBytes {
		return rejected
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != appealTokenPrefix {
		return rejected
	}

	signature, ok := decodeSegment(parts[2])
	if !ok || len(signature) != sha256.Size {
		return rejected
	}

	unsigned := parts[0] + "." + parts[1]
	if !hmac.Equal(signature, s.mac(unsigned)) {
		return rejected
	}

	payload, ok := decodeSegment(parts[1])
	if !ok {
		return rejected
	}

	var claims appealClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return rejected
	}

	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(payload, canonical) {
		return rejected
	}

	if claims.Version != 1 || claims.Purpose != service.AppealConfirmationPurpose || claims.Issuer != s.issuer || claims.Audience != s.audience {
		return rejected
	}

	actual := service.AppealConfirmationBinding{
		UserID:         claims.UserID,
		ConversationID: claims.ConversationID,
		IdempotencyKey: claims.IdempotencyKey,
		RequestHash:    claims.RequestHash,
		PolicyDigest:   claims.PolicyDigest,
	}
	if actual != expected {
		return rejected
	}

	issuedAt := time.UnixMilli(claims.IssuedAtMS).UTC()
	expiresAt := time.UnixMilli(claims.ExpiresAtMS).UTC()

	if !validInstant(issuedAt) || !validInstant(expiresAt) || !expiresAt.Equal(
		issuedAt.Add(service.AppealConfirmationLifetime),
	) {
		return rejected
	}

	now, err := s.now()
	if err != nil {
		return err
	}

	if now.Before(issuedAt) || !now.Before(expiresAt) {
		return rejected
	}
	return ctx.Err()
}
