package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

type messageHTTPRequest struct {
	Content   string
	Confirmed *service.AgentConfirmation
}

type confirmedActionRequest struct {
	SubjectType       model.AppealSubjectType `json:"subject_type"`
	SubjectRef        string                  `json:"subject_ref"`
	Reason            string                  `json:"reason"`
	IntakeMode        model.AppealIntakeMode  `json:"intake_mode"`
	IdempotencyKey    string                  `json:"idempotency_key"`
	PolicyDigest      string                  `json:"policy_digest"`
	ConfirmationToken string                  `json:"confirmation_token"`
}

func strictHTTPObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	bad := service.ErrInvalidArgument
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, bad
	}

	known := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		known[name] = true
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, bad
	}

	fields := make(map[string]json.RawMessage, len(allowed))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, bad
		}

		key, ok := token.(string)
		if !ok || !known[key] {
			return nil, bad
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, bad
		}

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, bad
		}
		value = bytes.TrimSpace(value)
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return nil, bad
		}
		fields[key] = value
	}

	token, err = dec.Token()
	if err != nil || token != json.Delim('}') {
		return nil, bad
	}

	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, bad
	}

	return fields, nil
}

func decodeMessageRequest(c *gin.Context) (messageHTTPRequest, error) {
	var request messageHTTPRequest

	reader := http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
	raw, err := io.ReadAll(reader)
	if err != nil {
		return request, err
	}

	fields, err := strictHTTPObject(raw, "content", "confirmed_action")
	if err != nil || len(fields) != 1 {
		return request, service.ErrInvalidArgument
	}

	if content, ok := fields["content"]; ok {
		if err := json.Unmarshal(content, &request.Content); err != nil {
			return request, service.ErrInvalidArgument
		}
		return request, nil
	}

	actionRaw := fields["confirmed_action"]
	actionFields, err := strictHTTPObject(
		actionRaw,
		"subject_type",
		"subject_ref",
		"reason",
		"intake_mode",
		"idempotency_key",
		"policy_digest",
		"confirmation_token",
	)
	if err != nil || len(actionFields) != 7 {
		return request, service.ErrInvalidArgument
	}

	var action confirmedActionRequest
	if err := json.Unmarshal(actionRaw, &action); err != nil {
		return request, service.ErrInvalidArgument
	}

	if err := service.ValidateAppealOperationKey(action.IdempotencyKey); err != nil {
		return request, err
	}
	if err := service.ValidateAppealDigest(action.PolicyDigest); err != nil {
		return request, err
	}

	request.Confirmed = &service.AgentConfirmation{
		Input: service.CreateAppealInput{
			SubjectType: action.SubjectType,
			SubjectRef:  action.SubjectRef,
			Reason:      action.Reason,
			IntakeMode:  action.IntakeMode,
		},
		Confirmation: service.AppealConfirmationInput{
			IdempotencyKey: action.IdempotencyKey,
			PolicyDigest:   action.PolicyDigest,
			Token:          action.ConfirmationToken,
		},
	}
	return request, nil
}
