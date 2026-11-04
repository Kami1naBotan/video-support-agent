package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	contractRunID  = "11111111-1111-4111-8111-111111111111"
	contractCallID = "22222222-2222-4222-8222-222222222222"
)

func contractDecodeRequest(raw string) (messageHTTPRequest, error) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages",
		strings.NewReader(raw),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	return decodeMessageRequest(ctx)
}

func contractActionJSON() string {
	return `{"subject_type":"account","subject_ref":"fixture-account",` +
		`"reason":"fixture reason","intake_mode":"manual_review",` +
		`"idempotency_key":"` + contractRunID + `",` +
		`"policy_digest":"` + strings.Repeat("a", 64) + `",` +
		`"confirmation_token":"fixture-only-not-a-real-token"}`
}

func contractJSONObject(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal public projection: %v", err)
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode public projection: %v", err)
	}
	return result
}

func TestAgentHTTPContractRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("text_preserved", func(t *testing.T) {
		got, err := contractDecodeRequest(`{"content":"  核对规则\n  "}`)
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "  核对规则\n  " || got.Confirmed != nil {
			t.Fatal("text request changed or entered confirmation mode")
		}
	})

	t.Run("confirmation_preserves_binding", func(t *testing.T) {
		got, err := contractDecodeRequest(
			`{"confirmed_action":` + contractActionJSON() + `}`,
		)
		if err != nil {
			t.Fatal(err)
		}
		if got.Content != "" || got.Confirmed == nil {
			t.Fatal("confirmation request entered wrong mode")
		}

		action := got.Confirmed
		if action.Input.SubjectType != model.AppealSubjectAccount ||
			action.Input.SubjectRef != "fixture-account" ||
			action.Input.Reason != "fixture reason" ||
			action.Input.IntakeMode != model.AppealIntakeManualReview ||
			action.Confirmation.IdempotencyKey != contractRunID ||
			action.Confirmation.PolicyDigest != strings.Repeat("a", 64) ||
			action.Confirmation.Token != "fixture-only-not-a-real-token" {
			t.Fatal("confirmation payload or binding changed")
		}
	})

	action := contractActionJSON()

	bad := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"empty_object", `{}`},
		{"null", `null`},
		{"array", `[]`},
		{"string_root", `"hello"`},
		{"null_content", `{"content":null}`},
		{"number_content", `{"content":123}`},
		{"unknown_identity", `{"content":"hello","user_id":7}`},
		{"wrong_case", `{"Content":"hello"}`},
		{"duplicate_content", `{"content":"a","content":"b"}`},
		{"escaped_duplicate", `{"content":"a","\u0063ontent":"b"}`},
		{"trailing_object", `{"content":"a"}{}`},
		{"trailing_literal", `{"content":"a"}true`},
		{"truncated", `{"content":`},
		{"mixed_modes", `{"content":"a","confirmed_action":` + action + `}`},
		{"null_action", `{"confirmed_action":null}`},
		{"array_action", `{"confirmed_action":[]}`},
		{"missing_action_fields", `{"confirmed_action":{}}`},
		{"nested_unknown", `{"confirmed_action":` +
			strings.Replace(action, `{`, `{"user_id":7,`, 1) + `}`},
		{"nested_duplicate", `{"confirmed_action":` +
			strings.Replace(action, `{`, `{"reason":"other",`, 1) + `}`},
		{"nested_wrong_case", `{"confirmed_action":` +
			strings.Replace(action, `"reason"`, `"Reason"`, 1) + `}`},
		{"nested_null", `{"confirmed_action":` +
			strings.Replace(action, `"fixture reason"`, `null`, 1) + `}`},
		{"nested_wrong_type", `{"confirmed_action":` +
			strings.Replace(action, `"fixture reason"`, `123`, 1) + `}`},
		{"invalid_key", `{"confirmed_action":` +
			strings.Replace(action, contractRunID, "not-a-uuid", 1) + `}`},
		{"uppercase_digest", `{"confirmed_action":` +
			strings.Replace(action, strings.Repeat("a", 64),
				strings.Repeat("A", 64), 1) + `}`},
		{"short_digest", `{"confirmed_action":` +
			strings.Replace(action, strings.Repeat("a", 64), "abc", 1) + `}`},
		{"invalid_utf8", "{\"content\":\"" + string([]byte{0xff}) + "\"}"},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := contractDecodeRequest(tc.raw)
			if !errors.Is(err, service.ErrInvalidArgument) {
				t.Fatalf("want invalid argument, got %v", err)
			}
		})
	}

	t.Run("body_limit", func(t *testing.T) {
		const limit = 64 * 1024
		overhead := len(`{"content":""}`)
		exact := `{"content":"` +
			strings.Repeat("x", limit-overhead) + `"}`

		if len(exact) != limit {
			t.Fatal("invalid boundary fixture")
		}

		if _, err := contractDecodeRequest(exact); err != nil {
			t.Fatalf("decoder rejected exact byte boundary: %v", err)
		}

		_, err := contractDecodeRequest(exact + " ")
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) || tooLarge.Limit != limit {
			t.Fatalf("want MaxBytesError at %d, got %v", limit, err)
		}
	})
}

func TestAgentHTTPContractToolEvent(t *testing.T) {
	t.Run("agent_start_has_only_run", func(t *testing.T) {
		kind, payload, err := projectAgentStreamEvent(service.AgentEvent{
			Kind:  "agent_start",
			RunID: contractRunID,
		})
		if err != nil || kind != SSEEventAgentStart {
			t.Fatalf("unexpected start projection: kind=%s err=%v", kind, err)
		}

		fields := contractJSONObject(t, payload)
		if len(fields) != 1 || string(fields["run_id"]) != `"`+contractRunID+`"` {
			t.Fatal("agent_start exposes unexpected fields")
		}
	})

	t.Run("tool_start_omits_end_fields", func(t *testing.T) {
		kind, payload, err := projectAgentStreamEvent(service.AgentEvent{
			Kind:           "tool_start",
			RunID:          contractRunID,
			CallID:         contractCallID,
			Sequence:       1,
			ToolName:       "search_rule",
			Status:         "started",
			BusinessEffect: "none",
		})
		if err != nil || kind != SSEEventToolStart {
			t.Fatalf("unexpected tool start: kind=%s err=%v", kind, err)
		}

		fields := contractJSONObject(t, payload)
		for _, forbidden := range []string{
			"duration_ms", "error_code", "ticket_id",
			"arguments", "confirmation_token",
		} {
			if _, exists := fields[forbidden]; exists {
				t.Fatalf("tool_start exposes %s", forbidden)
			}
		}
		if string(fields["replayed"]) != "false" {
			t.Fatal("tool_start must explicitly serialize replayed=false")
		}
	})

	duration := int64(0)
	base := service.AgentEvent{
		Kind:           "tool_end",
		RunID:          contractRunID,
		CallID:         contractCallID,
		Sequence:       1,
		ToolName:       "create_appeal_ticket",
		Status:         "succeeded",
		ErrorCode:      "none",
		BusinessEffect: "committed",
		TicketID:       "18446744073709551615",
		Replayed:       true,
		DurationMS:     &duration,
	}

	t.Run("committed_ticket_stays_string", func(t *testing.T) {
		kind, payload, err := projectAgentStreamEvent(base)
		if err != nil || kind != SSEEventToolEnd {
			t.Fatalf("unexpected tool end: kind=%s err=%v", kind, err)
		}

		fields := contractJSONObject(t, payload)
		if string(fields["ticket_id"]) != `"18446744073709551615"` ||
			string(fields["duration_ms"]) != "0" ||
			string(fields["replayed"]) != "true" ||
			string(fields["error_code"]) != `"none"` {
			t.Fatal("tool end lost ticket precision, replay or duration")
		}
	})

	mutations := []struct {
		name string
		edit func(*service.AgentEvent)
	}{
		{"unknown_kind", func(e *service.AgentEvent) { e.Kind = "surprise" }},
		{"bad_run", func(e *service.AgentEvent) { e.RunID = "bad" }},
		{"bad_call", func(e *service.AgentEvent) { e.CallID = "bad" }},
		{"zero_sequence", func(e *service.AgentEvent) { e.Sequence = 0 }},
		{"over_budget_sequence", func(e *service.AgentEvent) { e.Sequence = 6 }},
		{"unknown_tool", func(e *service.AgentEvent) { e.ToolName = "shell" }},
		{"nonterminal_status", func(e *service.AgentEvent) { e.Status = "started" }},
		{"unknown_effect", func(e *service.AgentEvent) { e.BusinessEffect = "maybe" }},
		{"unknown_error", func(e *service.AgentEvent) { e.ErrorCode = "raw SQL error" }},
		{"missing_duration", func(e *service.AgentEvent) { e.DurationMS = nil }},
		{"negative_duration", func(e *service.AgentEvent) {
			value := int64(-1)
			e.DurationMS = &value
		}},
		{"missing_ticket", func(e *service.AgentEvent) { e.TicketID = "" }},
		{"zero_ticket", func(e *service.AgentEvent) { e.TicketID = "0" }},
		{"noncanonical_ticket", func(e *service.AgentEvent) { e.TicketID = "01" }},
		{"overflow_ticket", func(e *service.AgentEvent) {
			e.TicketID = "18446744073709551616"
		}},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			tc.edit(&input)
			_, _, err := projectAgentStreamEvent(input)

			var execution *service.AgentExecutionError
			if !errors.As(err, &execution) || execution.Code != "protocol_error" {
				t.Fatalf("want protocol_error, got %v", err)
			}
		})
	}
}

func TestAgentHTTPContractResultProjection(t *testing.T) {
	if projectAgentResult(nil) != nil {
		t.Fatal("nil outcome must remain absent")
	}

	out := &service.AgentOutcome{
		RunID:             contractRunID,
		ExecutionStatus:   "failed",
		Decision:          "failed",
		ReasonCode:        "audit_unavailable",
		BusinessEffect:    "committed",
		MessageSaveStatus: "unknown",
		AuditPending:      true,
		Content:           "工单已提交，消息保存待核对",
		Ticket: &service.AppealCreateResult{
			TicketID:      ^uint64(0),
			InitialStatus: model.AppealTicketPendingReview,
			CreatedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			Replayed:      true,
		},
		Proposal: &service.PreparedAppeal{
			Token: "fixture-private-confirmation",
		},
	}

	view := projectAgentResult(out)
	if view.ExecutionStatus != "failed" ||
		view.BusinessEffect != "committed" ||
		view.MessageSaveStatus != "unknown" ||
		!view.AuditPending ||
		view.Ticket == nil ||
		view.Ticket.TicketID != "18446744073709551615" {
		t.Fatal("projection discarded committed facts on failure")
	}

	fields := contractJSONObject(t, view)
	if string(fields["citations"]) != "[]" ||
		string(fields["tools"]) != "[]" {
		t.Fatal("empty collections must serialize as arrays")
	}

	allowed := map[string]bool{
		"run_id": true, "execution_status": true, "decision": true,
		"reason_code": true, "business_effect": true,
		"message_save_status": true, "audit_pending": true,
		"content": true, "ticket": true, "ticket_status": true,
		"citations": true, "tools": true,
	}
	for key := range fields {
		if !allowed[key] {
			t.Fatalf("unexpected public field: %s", key)
		}
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fixture-private-confirmation") {
		t.Fatal("result projection leaked confirmation token")
	}

	if projectAppealAction(out) != nil {
		t.Fatal("committed failure must not expose a confirmation card")
	}

	before := *out.Ticket
	view.Ticket.Replayed = false
	if !reflect.DeepEqual(*out.Ticket, before) {
		t.Fatal("public ticket projection aliases source ticket")
	}
}

func TestAgentHTTPContractErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		input  error
		status int
		code   string
	}{
		{"save", service.ErrAgentMessageSave, 503, "agent_message_save_unconfirmed"},
		{"run_missing", service.ErrAgentRunNotFound, 404, "agent_resource_not_found"},
		{"audit", service.ErrAgentAuditUnavailable, 503, "agent_audit_unavailable"},
		{"conflict", service.ErrAppealIdempotencyConflict, 409, "appeal_idempotency_conflict"},
		{"confirmation", service.ErrAppealConfirmationRequired, 409, "appeal_confirmation_required"},
		{"changed", service.ErrAppealPreconditionChanged, 409, "appeal_precondition_changed"},
		{"ineligible", service.ErrAppealIneligible, 422, "appeal_ineligible"},
		{"unknown", service.ErrAppealOutcomeUnknown, 503, "appeal_outcome_unknown"},
		{"timeout", &service.AgentExecutionError{Code: "timed_out"}, 504, "agent_timeout"},
		{"cancelled", &service.AgentExecutionError{Code: "cancelled"}, 408, "agent_cancelled"},
		{"protocol", &service.AgentExecutionError{Code: "protocol_error"}, 502, "agent_invalid_result"},
		{"model", &service.AgentExecutionError{Code: "model_failed"}, 502, "agent_model_failed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, response, handled := classifyAgentResponseError(
				fmt.Errorf("fixture-private-detail: %w", tc.input),
			)

			if !handled || status != tc.status || response.Code != tc.code {
				t.Fatalf(
					"got handled=%v status=%d code=%s",
					handled, status, response.Code,
				)
			}
			if strings.TrimSpace(response.Message) == "" ||
				strings.Contains(response.Message, "fixture-private-detail") {
				t.Fatal("missing safe message or leaked wrapped error")
			}
		})
	}

	t.Run("unrelated_error_delegates", func(t *testing.T) {
		_, _, handled := classifyAgentResponseError(errors.New("unrelated"))
		if handled {
			t.Fatal("unrelated error must remain for common classification")
		}
	})
}
