package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/model"
)

const (
	MaxAppealSubjectRefRunes = 128
	MaxAppealReasonRunes     = 2000
	maxAppealRawFieldBytes   = 16 << 10
)

var (
	appealKeyPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	appealDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AppealFacts struct {
	SubjectType model.AppealSubjectType `json:"subject_type"`
	SubjectRef  string                  `json:"subject_ref"`
	Reason      string                  `json:"reason"`
}

type CreateAppealInput struct {
	SubjectType model.AppealSubjectType `json:"subject_type"`
	SubjectRef  string                  `json:"subject_ref"`
	Reason      string                  `json:"reason"`
	IntakeMode  model.AppealIntakeMode  `json:"intake_mode"`
}

func NormalizeAppealFacts(raw AppealFacts) (AppealFacts, []string, error) {
	switch raw.SubjectType {
	case model.AppealSubjectAccount, model.AppealSubjectContent, model.AppealSubjectOther:
	default:
		return AppealFacts{}, nil, fmt.Errorf("%w: subject_type", ErrInvalidArgument)
	}

	ref, err := normalizeAppealText(raw.SubjectRef, MaxAppealSubjectRefRunes, false)
	if err != nil {
		return AppealFacts{}, nil, fmt.Errorf("%w: subject_ref", ErrInvalidArgument)
	}
	reason, err := normalizeAppealText(raw.Reason, MaxAppealReasonRunes, true)
	if err != nil {
		return AppealFacts{}, nil, fmt.Errorf("%w: reason", ErrInvalidArgument)
	}

	missing := make([]string, 0, 2)
	if ref == "" {
		missing = append(missing, "subject_ref")
	}
	if reason == "" {
		missing = append(missing, "reason")
	}
	return AppealFacts{SubjectType: raw.SubjectType, SubjectRef: ref, Reason: reason}, missing, nil
}

func normalizeAppealText(raw string, limit int, normalizeNewlines bool) (string, error) {
	if len(raw) > maxAppealRawFieldBytes || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return "", ErrInvalidArgument
	}
	if normalizeNewlines {
		raw = strings.ReplaceAll(raw, "\r\n", "\n")
	}
	value := strings.TrimSpace(raw)
	if utf8.RuneCountInString(value) > limit {
		return "", ErrInvalidArgument
	}
	return value, nil
}

func NormalizeCreateAppealInput(raw CreateAppealInput) (CreateAppealInput, error) {
	facts, missing, err := NormalizeAppealFacts(AppealFacts{SubjectType: raw.SubjectType, SubjectRef: raw.SubjectRef, Reason: raw.Reason})
	if err != nil {
		return CreateAppealInput{}, err
	}
	if len(missing) != 0 {
		return CreateAppealInput{}, fmt.Errorf("%w: %s", ErrAppealNeedsMoreInfo, strings.Join(missing, ","))
	}
	switch raw.IntakeMode {
	case model.AppealIntakeStandard, model.AppealIntakeManualReview:
	default:
		return CreateAppealInput{}, fmt.Errorf("%w: intake_mode", ErrInvalidArgument)
	}
	return CreateAppealInput{
		SubjectType: facts.SubjectType, SubjectRef: facts.SubjectRef,
		Reason: facts.Reason, IntakeMode: raw.IntakeMode,
	}, nil
}

func ValidateAppealOperationKey(key string) error {
	if !appealKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: idempotency_key must be a canonical UUIDv4", ErrInvalidArgument)
	}
	return nil
}

func ValidateAppealDigest(value string) error {
	if !appealDigestPattern.MatchString(value) {
		return fmt.Errorf("%w: digest must contain 64 lowercase hexadecimal characters", ErrInvalidArgument)
	}
	return nil
}

func AppealRequestHash(conversationID uint64, raw CreateAppealInput) (string, error) {
	if conversationID == 0 {
		return "", fmt.Errorf("%w: conversation_id", ErrInvalidArgument)
	}
	input, err := NormalizeCreateAppealInput(raw)
	if err != nil {
		return "", err
	}
	canonical := struct {
		SchemaVersion  uint16                  `json:"schema_version"`
		ConversationID uint64                  `json:"conversation_id"`
		SubjectType    model.AppealSubjectType `json:"subject_type"`
		SubjectRef     string                  `json:"subject_ref"`
		Reason         string                  `json:"reason"`
		IntakeMode     model.AppealIntakeMode  `json:"intake_mode"`
	}{1, conversationID, input.SubjectType, input.SubjectRef, input.Reason, input.IntakeMode}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode appeal request: %w", err)
	}
	return appealSHA256(data), nil
}

func appealSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
