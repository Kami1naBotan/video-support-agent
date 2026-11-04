package handler

import (
	"context"
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

// 嵌入接口仅用于满足未使用的方法。
// 若接线意外调用未覆盖的方法，测试会失败，不会静默返回成功。
type wiringFixture struct {
	service.MessageStore
	service.ConversationStore
	service.AgentExecutor

	t *testing.T

	owner uint64
	rows  []model.Message
	out   *service.AgentOutcome
	seen  service.AgentRequest

	executions int
	recent     int
	listed     int
	marks      int
	pairs      int
	recoveries int
	batches    [][]string

	pairErr     error
	markErr     error
	receiptErr  error
	receipt     *service.AgentMessageReceipt
	observation *service.AgentAuditObservation

	beforeSave func(context.Context)
	runFilter  func([]model.AgentRun) []model.AgentRun

	messages *service.MessageService
	reads    *service.AgentReadService
	router   *gin.Engine
	token    string
}

func newWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()

	f := &wiringFixture{
		t:     t,
		owner: 7,
		rows: []model.Message{
			{ID: 1, ConversationID: 1, Role: "user", Content: "previous question"},
			{ID: 2, ConversationID: 1, Role: "assistant", Content: "previous answer"},
		},
		out: &service.AgentOutcome{
			RunID:             contractRunID,
			ExecutionStatus:   "completed",
			Decision:          "answer",
			ReasonCode:        "none",
			BusinessEffect:    "none",
			MessageSaveStatus: "not_saved",
			Content:           "fixture answer",
		},
	}
	f.observation = &service.AgentAuditObservation{
		Stored: &service.AgentAuditSnapshot{
			Run: wiringRun(contractRunID),
		},
		BusinessEffect: "none",
		RecoveryCode:   "none",
	}

	var err error
	f.messages, err = service.NewAgentMessageService(f, f, nil, nil, f, f)
	if err != nil {
		t.Fatal(err)
	}
	f.reads, err = service.NewAgentReadService(f, f, f, f)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewAgentMessageHandler(f.messages, f.reads)
	if err != nil {
		t.Fatal(err)
	}

	f.router, f.token = newMessageTestRouter(t)
	f.router.POST("/conversations/:id/messages", h.Create)
	f.router.POST("/conversations/:id/messages/stream", h.CreateStream)
	f.router.GET("/conversations/:id/messages", h.List)
	f.router.GET("/conversations/:id/agent-runs/:run_id", h.GetAgentRun)
	return f
}

func wiringRun(id string) model.AgentRun {
	return model.AgentRun{
		ID:                id,
		UserID:            7,
		ConversationID:    1,
		ExecutionStatus:   "completed",
		Decision:          "answer",
		ReasonCode:        "none",
		BusinessEffect:    "none",
		MessageSaveStatus: "not_saved",
	}
}

func (f *wiringFixture) checkOwner(owner service.AgentAuditOwner) {
	f.t.Helper()
	if owner.UserID != 7 || owner.ConversationID != 1 {
		f.t.Fatalf("unexpected audit owner: %+v", owner)
	}
}

func (f *wiringFixture) GetByID(
	_ context.Context, id uint64,
) (*model.Conversation, error) {
	if id != 1 {
		f.t.Fatalf("unexpected conversation ID: %d", id)
	}
	return &model.Conversation{ID: id, UserID: f.owner}, nil
}

func (f *wiringFixture) ListRecentByConversationID(
	_ context.Context, id uint64, limit int,
) ([]model.Message, error) {
	if id != 1 || limit <= 0 {
		f.t.Fatalf("invalid recent-history query: %d/%d", id, limit)
	}
	f.recent++
	return f.rows, nil
}

func (f *wiringFixture) ListByConversationID(
	_ context.Context, id uint64,
) ([]model.Message, error) {
	if id != 1 {
		f.t.Fatalf("unexpected history conversation: %d", id)
	}
	f.listed++
	return f.rows, nil
}

func (f *wiringFixture) CreatePair(
	ctx context.Context, user, assistant *model.Message,
) error {
	f.pairs++
	if ctx.Err() != nil {
		f.t.Fatalf("save received cancelled context: %v", ctx.Err())
	}
	if user.ConversationID != 1 || assistant.ConversationID != 1 ||
		user.Role != "user" || assistant.Role != "assistant" ||
		user.AgentRunID == nil || assistant.AgentRunID == nil ||
		*user.AgentRunID != contractRunID ||
		*assistant.AgentRunID != contractRunID {
		f.t.Fatal("message pair lost conversation, role or Run association")
	}
	if f.beforeSave != nil {
		f.beforeSave(ctx)
	}
	if f.pairErr != nil {
		return f.pairErr
	}
	user.ID = 10
	assistant.ID = 11
	f.rows = append(f.rows, *user, *assistant)
	return nil
}

func (f *wiringFixture) MarkMessageSaveUnknown(
	ctx context.Context, owner service.AgentAuditOwner, runID string,
) error {
	f.checkOwner(owner)
	if runID != contractRunID || ctx.Err() != nil {
		f.t.Fatal("invalid save-state context or Run")
	}
	f.marks++
	return f.markErr
}

func (f *wiringFixture) ReadMessageReceipt(
	_ context.Context, owner service.AgentAuditOwner, runID string,
) (*service.AgentMessageReceipt, error) {
	f.checkOwner(owner)
	if runID != contractRunID {
		f.t.Fatalf("unexpected receipt Run: %s", runID)
	}
	return f.receipt, f.receiptErr
}

func (f *wiringFixture) ListRunsOwned(
	_ context.Context, owner service.AgentAuditOwner, ids []string,
) ([]model.AgentRun, error) {
	f.checkOwner(owner)
	f.batches = append(f.batches, append([]string(nil), ids...))

	runs := make([]model.AgentRun, 0, len(ids))
	for _, id := range ids {
		runs = append(runs, wiringRun(id))
	}
	if f.runFilter != nil {
		runs = f.runFilter(runs)
	}
	return runs, nil
}

func (f *wiringFixture) Recover(
	_ context.Context, owner service.AgentAuditOwner, runID string,
) (*service.AgentAuditObservation, error) {
	f.checkOwner(owner)
	if runID != contractRunID {
		f.t.Fatalf("unexpected recovery Run: %s", runID)
	}
	f.recoveries++
	return f.observation, nil
}

func (f *wiringFixture) Execute(
	ctx context.Context, request service.AgentRequest, sink service.AgentEventSink,
) (*service.AgentOutcome, error) {
	f.executions++
	f.seen = request

	duration := int64(1)
	events := []service.AgentEvent{
		{Kind: "agent_start", RunID: contractRunID},
		{
			Kind: "tool_start", RunID: contractRunID,
			CallID: contractCallID, Sequence: 1, ToolName: "search_rule",
			Status: "started", BusinessEffect: "none",
		},
		{
			Kind: "tool_end", RunID: contractRunID,
			CallID: contractCallID, Sequence: 1, ToolName: "search_rule",
			Status: "succeeded", BusinessEffect: "none",
			ErrorCode: "none", DurationMS: &duration,
		},
	}
	for _, event := range events {
		if err := sink(ctx, event); err != nil {
			if ctx.Err() == nil {
				f.t.Fatal("delivery failure did not cancel execution context")
			}
			return f.out, err
		}
	}
	return f.out, nil
}

func (f *wiringFixture) serve(
	writer http.ResponseWriter, method, path, body string, authenticated bool,
) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+f.token)
	}
	f.router.ServeHTTP(writer, request)
}

func wiringDecode[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode response: %v\n%s", err, raw)
	}
	return value
}

func TestAgentWiringHTTP(t *testing.T) {
	t.Run("trusted_identity_history_and_compatible_response", func(t *testing.T) {
		f := newWiringFixture(t)
		w := httptest.NewRecorder()
		f.serve(w, "POST", "/conversations/1/messages", `{"content":"  hello  "}`, true)

		if w.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		got := wiringDecode[agentMessageResponse](t, w.Body.String())
		if got.Message == nil || got.ID != 10 || got.Content != "hello" ||
			got.Agent == nil || got.Agent.RunID != contractRunID ||
			got.Agent.MessageSaveStatus != "saved" ||
			got.Messages == nil || got.Messages.AssistantMessageID != 11 {
			t.Fatalf("incomplete response: %s", w.Body.String())
		}
		if f.seen.UserID != 7 || f.seen.ConversationID != 1 ||
			f.seen.Question != "hello" || len(f.seen.History) != 2 ||
			f.seen.History[0].Content != "previous question" ||
			f.seen.History[1].Content != "previous answer" ||
			f.executions != 1 || f.recent != 1 || f.pairs != 1 || f.marks != 1 {
			t.Fatal("trusted identity/history/save wiring failed")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("response is cacheable")
		}
	})

	t.Run("confirmation_bypasses_history_and_preserves_binding", func(t *testing.T) {
		f := newWiringFixture(t)
		raw := `{"confirmed_action":` + contractActionJSON() + `}`
		expected, err := contractDecodeRequest(raw)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		f.serve(w, "POST", "/conversations/1/messages", raw, true)

		if w.Code != http.StatusCreated || f.recent != 0 ||
			f.seen.Question != "" || len(f.seen.History) != 0 ||
			!reflect.DeepEqual(f.seen.Confirmed, expected.Confirmed) {
			t.Fatalf("confirmation wiring failed: %d %s", w.Code, w.Body.String())
		}
		if f.rows[len(f.rows)-2].Content != "确认提交本项目工单申请。" ||
			strings.Contains(w.Body.String(), "fixture-only-not-a-real-token") {
			t.Fatal("confirmation secret leaked into saved message or response")
		}
	})

	t.Run("save_failure_preserves_business_fact", func(t *testing.T) {
		f := newWiringFixture(t)
		f.out.BusinessEffect = "committed"
		f.pairErr = errors.New("fixture-private-database-error")
		w := httptest.NewRecorder()
		f.serve(w, "POST", "/conversations/1/messages", `{"content":"hello"}`, true)

		got := wiringDecode[agentMessageResponse](t, w.Body.String())
		if w.Code != http.StatusServiceUnavailable ||
			got.Code != "agent_message_save_unconfirmed" ||
			got.Agent == nil || got.Agent.BusinessEffect != "committed" ||
			got.Agent.MessageSaveStatus != "unknown" || got.Messages != nil ||
			strings.Contains(w.Body.String(), "fixture-private") {
			t.Fatalf("save error lost facts or leaked internals: %s", w.Body.String())
		}
	})

	t.Run("receipt_restores_saved_response", func(t *testing.T) {
		f := newWiringFixture(t)
		f.pairErr = errors.New("uncertain save response")
		f.receipt = &service.AgentMessageReceipt{
			UserMessageID: 10, AssistantMessageID: 11,
		}
		runID := contractRunID
		f.rows = append(f.rows, model.Message{
			ID: 10, ConversationID: 1, Role: "user", Content: "hello",
			AgentRunID: &runID,
		})
		w := httptest.NewRecorder()
		f.serve(w, "POST", "/conversations/1/messages", `{"content":"hello"}`, true)

		got := wiringDecode[agentMessageResponse](t, w.Body.String())
		if w.Code != http.StatusCreated || got.Message == nil ||
			got.ID != 10 || got.Agent == nil ||
			got.Agent.MessageSaveStatus != "saved" ||
			got.Messages == nil || got.Messages.AssistantMessageID != 11 ||
			f.listed != 1 {
			t.Fatalf("receipt restoration failed: %s", w.Body.String())
		}
	})

	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{"POST", "/conversations/1/messages"},
		{"POST", "/conversations/1/messages/stream"},
		{"GET", "/conversations/1/messages"},
		{"GET", "/conversations/1/agent-runs/" + contractRunID},
	} {
		for _, authenticated := range []bool{false, true} {
			name := fmt.Sprintf("%s_%s_auth=%v", endpoint.method, endpoint.path, authenticated)
			t.Run(name, func(t *testing.T) {
				f := newWiringFixture(t)
				f.owner = 8
				w := &wiringWriter{ResponseRecorder: httptest.NewRecorder()}
				f.serve(w, endpoint.method, endpoint.path, `{"content":"hello"}`, authenticated)

				want := http.StatusUnauthorized
				if authenticated {
					want = http.StatusNotFound
				}
				if w.Code != want || f.executions != 0 || f.recent != 0 ||
					f.listed != 0 || f.recoveries != 0 || len(f.batches) != 0 ||
					f.marks != 0 || f.pairs != 0 {
					t.Fatalf("authorization boundary failed: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

// ResponseController 需要写截止时间和可返回错误的 Flush。
// 普通 ResponseRecorder 不满足前者，不能直接用于当前 SSE Handler。
type wiringWriter struct {
	*httptest.ResponseRecorder
	flushes int
	failAt  int
}

func (w *wiringWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *wiringWriter) FlushError() error {
	w.flushes++
	if w.failAt != 0 && w.flushes == w.failAt {
		return errors.New("fixture transport failure")
	}
	w.ResponseRecorder.Flush()
	return nil
}

type wiringFrame struct {
	kind string
	data string
}

func wiringFrames(t *testing.T, raw string) []wiringFrame {
	t.Helper()
	var frames []wiringFrame
	for _, block := range strings.Split(strings.TrimSpace(raw), "\n\n") {
		if block == "" {
			continue
		}
		frame := wiringFrame{}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event:") {
				frame.kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				frame.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if frame.kind == "" || !json.Valid([]byte(frame.data)) {
			t.Fatalf("invalid SSE frame: %q", block)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestAgentWiringSSE(t *testing.T) {
	for _, mode := range []string{"success", "save_failure", "confirmation_card", "send_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newWiringFixture(t)
			w := &wiringWriter{ResponseRecorder: httptest.NewRecorder()}
			if mode == "save_failure" {
				f.pairErr = errors.New("fixture save failure")
			}
			if mode == "confirmation_card" {
				request, err := contractDecodeRequest(
					`{"confirmed_action":` + contractActionJSON() + `}`,
				)
				if err != nil {
					t.Fatal(err)
				}
				f.out.Decision = "confirmation_required"
				f.out.Proposal = &service.PreparedAppeal{
					Input: request.Confirmed.Input,
					Token: "fixture-only-not-a-real-token",
				}
			}
			if mode == "send_failure" {
				// 第5帧是 agent_result；此时执行器已返回业务结果。
				w.failAt = 5
				f.out.BusinessEffect = "committed"
			}
			f.beforeSave = func(context.Context) {
				body := w.Body.String()
				if !strings.Contains(body, "event: agent_result") ||
					strings.Contains(body, "event: done") {
					t.Fatal("save must follow agent_result and precede done")
				}
			}

			f.serve(w, "POST", "/conversations/1/messages/stream", `{"content":"hello"}`, true)
			if w.Code != http.StatusOK ||
				!strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("stream did not start: %d %s", w.Code, w.Body.String())
			}
			frames := wiringFrames(t, w.Body.String())
			kinds := make([]string, 0, len(frames))
			for _, frame := range frames {
				kinds = append(kinds, frame.kind)
				if frame.kind == "agent_result" {
					result := wiringDecode[agentResultView](t, frame.data)
					if result.MessageSaveStatus == "saved" {
						t.Fatal("pre-save event claimed saved")
					}
				}
			}
			want := []string{"start", "agent_start", "tool_start", "tool_end"}
			if mode == "confirmation_card" {
				want = append(want, "action_required")
			}
			want = append(want, "agent_result")
			if mode != "send_failure" {
				want = append(want, "token")
				if mode == "save_failure" {
					want = append(want, "error")
				} else {
					want = append(want, "citation", "done")
				}
			}
			if !reflect.DeepEqual(kinds, want) || f.pairs != 1 {
				t.Fatalf("events=%v want=%v saves=%d", kinds, want, f.pairs)
			}

			last := frames[len(frames)-1]
			switch mode {
			case "save_failure":
				got := wiringDecode[agentStreamError](t, last.data)
				if got.Code != "agent_message_save_unconfirmed" ||
					got.Agent == nil || got.Agent.MessageSaveStatus != "unknown" ||
					got.Messages != nil {
					t.Fatalf("invalid stream error: %s", last.data)
				}
			case "send_failure":
				if f.out.BusinessEffect != "committed" ||
					f.out.MessageSaveStatus != "saved" {
					t.Fatal("send failure erased committed/saved facts")
				}
			default:
				got := wiringDecode[agentStreamDone](t, last.data)
				if got.RunID != contractRunID || got.UserMessageID != 10 ||
					got.AssistantMessageID != 11 || got.MessageSaveStatus != "saved" ||
					got.Agent == nil || got.Agent.MessageSaveStatus != "saved" {
					t.Fatalf("invalid done: %s", last.data)
				}
			}
			if mode == "confirmation_card" {
				card := wiringDecode[appealActionView](t, frames[4].data)
				if card.RunID != contractRunID ||
					!strings.Contains(frames[4].data, "fixture-only-not-a-real-token") {
					t.Fatal("confirmation card missing its binding payload")
				}
				for _, frame := range frames {
					if frame.kind != "action_required" &&
						strings.Contains(frame.data, "fixture-only-not-a-real-token") {
						t.Fatal("confirmation token escaped action_required")
					}
				}
			}
		})
	}
}

func TestAgentWiringCallbacks(t *testing.T) {
	for _, stage := range []string{"start", "event", "result", "chunk"} {
		for _, effect := range []string{"none", "unknown", "committed"} {
			t.Run(stage+"/"+effect, func(t *testing.T) {
				f := newWiringFixture(t)
				f.out.BusinessEffect = effect
				deliveryErr := errors.New("fixture delivery failure")
				var callbackDeadline time.Time
				calls := []string{}

				callback := func(ctx context.Context, name string) error {
					calls = append(calls, name)
					if name == "result" || name == "chunk" {
						deadline, ok := ctx.Deadline()
						if !ok {
							t.Fatal("delivery has no deadline")
						}
						if !callbackDeadline.IsZero() && !deadline.Equal(callbackDeadline) {
							t.Fatal("result and chunk received separate finalization budgets")
						}
						callbackDeadline = deadline
					}
					if name == stage {
						return deliveryErr
					}
					return nil
				}
				f.beforeSave = func(ctx context.Context) {
					if !callbackDeadline.IsZero() {
						deadline, ok := ctx.Deadline()
						if !ok || !deadline.Equal(callbackDeadline) {
							t.Fatal("save received a fresh finalization budget")
						}
					}
				}
				result, err := f.messages.CreateAgent(
					context.Background(),
					service.AgentMessageInput{UserID: 7, ConversationID: 1, Content: "hello"},
					service.AgentMessageCallbacks{
						OnStart: func() error {
							return callback(context.Background(), "start")
						},
						OnEvent: func(ctx context.Context, _ service.AgentEvent) error {
							return callback(ctx, "event")
						},
						OnResult: func(ctx context.Context, _ *service.AgentOutcome) error {
							return callback(ctx, "result")
						},
						OnChunk: func(ctx context.Context, _ string) error {
							return callback(ctx, "chunk")
						},
					},
				)
				if !errors.Is(err, deliveryErr) {
					t.Fatalf("delivery failure not retained: %v", err)
				}
				want := []string{"start"}
				if stage != "start" {
					want = append(want, "event")
					if stage != "event" {
						want = append(want, "event", "event", "result")
						if stage == "chunk" {
							want = append(want, "chunk")
						}
					}
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("callbacks=%v want=%v", calls, want)
				}
				shouldSave := stage != "start" && effect != "none"
				if shouldSave {
					if f.pairs != 1 || result == nil || result.Receipt == nil ||
						result.Outcome.BusinessEffect != effect ||
						result.Outcome.MessageSaveStatus != "saved" {
						t.Fatal("business-effect result was not finalized")
					}
				} else if f.pairs != 0 || f.marks != 0 {
					t.Fatal("unexpected save after delivery failure")
				}
				if stage == "start" && f.executions != 0 {
					t.Fatal("executor ran after start failure")
				}
			})
		}
	}
}

func TestAgentWiringReads(t *testing.T) {
	t.Run("history_batches_deduplicates_and_projects", func(t *testing.T) {
		f := newWiringFixture(t)
		f.rows = nil
		for i := 1; i <= 51; i++ {
			id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
			f.rows = append(f.rows, model.Message{
				ID: uint64(i), ConversationID: 1, Role: "user",
				Content: "history", AgentRunID: &id,
			})
		}
		f.rows = append(f.rows, f.rows[0])
		w := httptest.NewRecorder()
		f.serve(w, "GET", "/conversations/1/messages", "", true)

		got := wiringDecode[[]service.MessageHistoryView](t, w.Body.String())
		if w.Code != http.StatusOK || len(got) != 52 ||
			len(f.batches) != 2 || len(f.batches[0]) != 50 ||
			len(f.batches[1]) != 1 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("history batching failed: status=%d batches=%v", w.Code, f.batches)
		}
		for i, message := range got {
			if message.Agent == nil || message.AgentRunID == nil ||
				message.Agent.RunID != *message.AgentRunID {
				t.Fatalf("missing history summary at %d", i)
			}
		}
	})

	for _, fault := range []string{
		"foreign_message", "invalid_run", "foreign_run", "missing_run", "duplicate_run",
	} {
		t.Run("reject_"+fault, func(t *testing.T) {
			f := newWiringFixture(t)
			id := contractRunID
			f.rows = []model.Message{{
				ID: 1, ConversationID: 1, Role: "user", AgentRunID: &id,
			}}
			switch fault {
			case "foreign_message":
				f.rows[0].ConversationID = 2
			case "invalid_run":
				id = "invalid"
			case "foreign_run":
				f.runFilter = func(runs []model.AgentRun) []model.AgentRun {
					runs[0].UserID = 8
					return runs
				}
			case "missing_run":
				f.runFilter = func([]model.AgentRun) []model.AgentRun { return nil }
			case "duplicate_run":
				f.runFilter = func(runs []model.AgentRun) []model.AgentRun {
					return append(runs, runs[0])
				}
			}
			got, err := f.reads.ListHistory(context.Background(), 7, 1)
			if !errors.Is(err, service.ErrAgentAuditUnavailable) || got != nil {
				t.Fatalf("unsafe history accepted: %+v %v", got, err)
			}
		})
	}

	t.Run("partial_recovery_preserves_observed_commit", func(t *testing.T) {
		f := newWiringFixture(t)
		f.observation.Stored.Run.ExecutionStatus = "running"
		f.observation.Stored.Run.BusinessEffect = "unknown"
		f.observation.BusinessEffect = "committed"
		f.observation.Ticket = &service.AppealCreateResult{
			TicketID: 42, InitialStatus: model.AppealTicketPendingReview,
		}
		f.receiptErr = fmt.Errorf("fixture-private: %w", service.ErrAgentAuditUnavailable)

		w := httptest.NewRecorder()
		f.serve(w, "GET", "/conversations/1/agent-runs/"+contractRunID, "", true)
		got := wiringDecode[struct {
			ErrorResponse
			Recovery *service.AgentRecoveryView `json:"recovery"`
		}](t, w.Body.String())

		if w.Code != http.StatusServiceUnavailable || got.Recovery == nil {
			t.Fatalf("missing partial recovery: %d %s", w.Code, w.Body.String())
		}
		view := got.Recovery
		if view.Stored.BusinessEffect != "unknown" ||
			view.BusinessEffect != "committed" || !view.AuditPending ||
			view.MessageSaveStatus != "unknown" || view.Messages != nil ||
			view.Ticket == nil || view.Ticket.TicketID != "42" ||
			strings.Contains(w.Body.String(), "fixture-private") ||
			w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("partial recovery corrupted facts: %s", w.Body.String())
		}
	})

	t.Run("recovery_receipt_confirms_saved", func(t *testing.T) {
		f := newWiringFixture(t)
		f.receipt = &service.AgentMessageReceipt{
			UserMessageID: 10, AssistantMessageID: 11,
		}
		w := httptest.NewRecorder()
		f.serve(w, "GET", "/conversations/1/agent-runs/"+contractRunID, "", true)
		got := wiringDecode[service.AgentRecoveryView](t, w.Body.String())
		if w.Code != http.StatusOK || got.MessageSaveStatus != "saved" ||
			got.Messages == nil || got.Messages.AssistantMessageID != 11 ||
			got.Stored.MessageSaveStatus != "not_saved" {
			t.Fatalf("stored state and receipt observation conflated: %s", w.Body.String())
		}
	})

	t.Run("foreign_recovered_run_is_rejected", func(t *testing.T) {
		f := newWiringFixture(t)
		f.observation.Stored.Run.UserID = 8
		got, err := f.reads.RecoverRun(context.Background(), 7, 1, contractRunID)
		if !errors.Is(err, service.ErrAgentAuditUnavailable) || got != nil {
			t.Fatalf("foreign recovery accepted: %+v %v", got, err)
		}
	})
}
