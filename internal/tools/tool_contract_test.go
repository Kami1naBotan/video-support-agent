package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
)

var toolAt = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)

const toolInput = `{"subject_type":"account","subject_ref":"test-account","reason":"test reason","intake_mode":"manual_review"}`

func toolCode(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	var f *Failure
	if !errors.As(err, &f) || f.Code != want || err.Error() != want {
		t.Fatalf("error=%v; want safe code %s", err, want)
	}
}

func toolJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func toolKeys(t *testing.T, raw string, want ...string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys=%v; want=%v", got, want)
	}
}

// 全部是虚构内存资料。URL只用于格式校验，不访问网络、不写知识库。
type toolRig struct {
	t         *testing.T
	caller    Caller
	session   *RuleSession
	calls     []string
	err       map[string]error
	after     func(string)
	conv      *model.Conversation
	ticket    *service.AppealTicketView
	retrieved retrieval.HybridResult
	current   *service.KnowledgeCurrentDetail
	history   *service.KnowledgeHistoryArticleDetail
	snapshot  model.AppealEligibilitySnapshot
	proposal  *service.PreparedAppeal
	result    *service.AppealCreateResult
	input     service.CreateAppealInput
	binding   service.AppealConfirmationBinding
	query     string
	topK      int
	facts     service.AppealFacts
}

func toolSnapshot(decision model.AppealEligibilityDecision) model.AppealEligibilitySnapshot {
	s := model.AppealEligibilitySnapshot{
		SchemaVersion: 1,
		Decision:      decision,
		ReasonCode:    "test_only",
		CheckedAt:     toolAt,
		FactSources: []model.AppealFactProvenance{
			{Field: "subject_type", Source: model.AppealFactUserReported},
			{Field: "subject_ref", Source: model.AppealFactUserReported},
			{Field: "reason", Source: model.AppealFactUserReported},
		},
	}
	if decision == model.AppealEligibilityNeedsMoreInfo {
		s.MissingFields = []string{"reason"}
		s.FactSources = s.FactSources[:2]
	}
	if decision == model.AppealEligibilityEligible ||
		decision == model.AppealEligibilityIneligible {
		s.PolicyCode = "TEST-ONLY"
		s.PolicyRevision = "v1"
		s.EvidenceRefs = []model.AppealRuleEvidence{{
			DocumentID: 21, VersionID: 31, ArticleID: 41, PublicationRevision: 1,
		}}
	}
	return s
}

func newToolRig(t *testing.T) *toolRig {
	t.Helper()
	r := &toolRig{t: t, err: map[string]error{}}
	var err error
	r.caller, err = NewCaller(7, 9)
	toolCode(t, err, "")
	r.session, err = NewRuleSession(r.caller, r)
	toolCode(t, err, "")

	r.conv = &model.Conversation{ID: 9, UserID: 7}
	r.ticket = &service.AppealTicketView{
		TicketID: 51, Status: model.AppealTicketPendingReview,
		SubjectRef: "private-test-subject", CreatedAt: toolAt, UpdatedAt: toolAt,
	}
	r.input = service.CreateAppealInput{
		SubjectType: model.AppealSubjectAccount,
		SubjectRef:  "test-account",
		Reason:      "test reason",
		IntakeMode:  model.AppealIntakeManualReview,
	}
	r.snapshot = toolSnapshot(model.AppealEligibilityManualReview)

	effective := toolAt.Add(-time.Hour)
	version := &model.RuleVersion{
		ID: 31, DocumentID: 21, Version: "test-v1", Title: "虚构测试规则",
		Content: "虚构测试全文", SourceURL: "https://www.bilibili.com/__tool_fixture__",
		Status:      model.RuleVersionStatusPublished,
		EffectiveAt: &effective, PublishedAt: &effective,
	}
	article := model.RuleArticle{
		ID: 41, VersionID: 31, ArticleOrder: 1, Content: "虚构测试条款",
	}
	r.current = &service.KnowledgeCurrentDetail{
		Version: version, Articles: []model.RuleArticle{article},
	}
	r.history = &service.KnowledgeHistoryArticleDetail{
		Version: version, Article: &article,
		State: service.KnowledgeHistoryState{IsSuperseded: true},
	}
	r.retrieved = retrieval.HybridResult{
		RetrievedAt: toolAt,
		Outcome:     retrieval.HybridSuccess,
		Stats:       retrieval.HybridStats{FinalCount: 1},
		Items: []retrieval.HybridItem{{
			Rule: retrieval.HydratedChunk{
				Chunk: retrieval.RuleChunk{
					ChunkID: 11, DocumentID: 21, VersionID: 31,
					ArticleID: 41, CategoryID: 1, ArticleOrder: 1,
					Version: "test-v1", Title: "虚构测试规则",
					Content: "虚构测试片段", SourceURL: version.SourceURL,
					EffectiveAt: effective,
				},
				ArticleContent: article.Content,
			},
			ChunkIDs: []uint64{11},
		}},
	}
	r.result = &service.AppealCreateResult{
		TicketID: 51, InitialStatus: model.AppealTicketPendingReview, CreatedAt: toolAt,
	}
	r.refreshProposal()
	return r
}

func (r *toolRig) refreshProposal() {
	hash, err := service.AppealRequestHash(9, r.input)
	toolCode(r.t, err, "")
	digest, err := service.AppealPolicyDigest(r.snapshot)
	toolCode(r.t, err, "")
	status, err := service.AppealInitialStatus(r.snapshot, r.input.IntakeMode)
	toolCode(r.t, err, "")

	r.binding = service.AppealConfirmationBinding{
		UserID: 7, ConversationID: 9,
		IdempotencyKey: "00000000-0000-4000-8000-000000000001",
		RequestHash:    hash, PolicyDigest: digest,
	}
	r.proposal = &service.PreparedAppeal{
		Input: r.input, Eligibility: r.snapshot, InitialStatus: status,
		Binding: r.binding, Token: "TEST-ONLY-NOT-A-REAL-SIGNATURE",
		IssuedAt: toolAt, ExpiresAt: toolAt.Add(10 * time.Minute),
	}
}

func (r *toolRig) hit(ctx context.Context, name string, ids ...uint64) error {
	r.t.Helper()
	if len(ids) > 0 && !reflect.DeepEqual(ids, []uint64{7, 9}) {
		r.t.Fatalf("identity=%v", ids)
	}
	deadline, ok := ctx.Deadline()
	limit := 8 * time.Second
	if name == "prepare" || name == "create" || name == "audit" {
		limit = 5 * time.Second
	}
	if !ok || time.Until(deadline) > limit {
		r.t.Fatalf("%s missing bounded deadline", name)
	}
	r.calls = append(r.calls, name)
	if r.after != nil {
		r.after(name)
	}
	return r.err[name]
}

func (r *toolRig) GetByID(ctx context.Context, u, c uint64) (*model.Conversation, error) {
	return r.conv, r.hit(ctx, "owner", u, c)
}

func (r *toolRig) Query(ctx context.Context, u, c, id uint64) (*service.AppealTicketView, error) {
	if id != 51 {
		r.t.Fatalf("ticket id=%d", id)
	}
	return r.ticket, r.hit(ctx, "query", u, c)
}

func (r *toolRig) Retrieve(ctx context.Context, q string, o retrieval.HybridOptions) (retrieval.HybridResult, error) {
	r.query, r.topK = q, o.TopK
	return r.retrieved, r.hit(ctx, "retrieve")
}

func (r *toolRig) GetCurrentByDocumentID(ctx context.Context, id uint64) (*service.KnowledgeCurrentDetail, error) {
	if id != 21 {
		r.t.Fatalf("document id=%d", id)
	}
	return r.current, r.hit(ctx, "current")
}

func (r *toolRig) GetHistoryArticle(ctx context.Context, v, a uint64) (*service.KnowledgeHistoryArticleDetail, error) {
	if v != 31 || a != 41 {
		r.t.Fatalf("history ids=%d/%d", v, a)
	}
	return r.history, r.hit(ctx, "history")
}

func (r *toolRig) CheckEligibility(ctx context.Context, u, c uint64, f service.AppealFacts) (model.AppealEligibilitySnapshot, error) {
	r.facts = f
	return r.snapshot, r.hit(ctx, "eligibility", u, c)
}

func (r *toolRig) Prepare(ctx context.Context, u, c uint64, in service.CreateAppealInput) (*service.PreparedAppeal, error) {
	if in != r.input {
		r.t.Fatal("prepare input not normalized")
	}
	return r.proposal, r.hit(ctx, "prepare", u, c)
}

func (r *toolRig) Create(ctx context.Context, u, c uint64, in service.CreateAppealInput, confirm service.AppealConfirmationInput) (*service.AppealCreateResult, error) {
	if in != r.input || confirm != r.confirmed().Confirmation {
		r.t.Fatal("create binding changed")
	}
	return r.result, r.hit(ctx, "create", u, c)
}

func (r *toolRig) BeforeCreate(ctx context.Context, b service.AppealConfirmationBinding) error {
	if b != r.binding {
		r.t.Fatal("audit binding changed")
	}
	return r.hit(ctx, "audit")
}

func (r *toolRig) confirmed() *ConfirmedAppeal {
	return &ConfirmedAppeal{
		Input: r.input,
		Confirmation: service.AppealConfirmationInput{
			IdempotencyKey: r.binding.IdempotencyKey,
			PolicyDigest:   r.binding.PolicyDigest,
			Token:          r.proposal.Token,
		},
	}
}

func (r *toolRig) registry(confirmed *ConfirmedAppeal) *Registry {
	reg, err := NewRegistry(r.caller, RegistryDependencies{
		Conversations: r, Retriever: r, Knowledge: r, Appeals: r, WriteAudit: r,
	}, confirmed)
	toolCode(r.t, err, "")
	return reg
}

func (r *toolRig) wantCalls(want ...string) {
	r.t.Helper()
	if strings.Join(r.calls, ",") != strings.Join(want, ",") {
		r.t.Fatalf("calls=%v; want=%v", r.calls, want)
	}
}

func TestToolContractParameters(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"empty", ""},
		{"array", "[]"},
		{"null", "null"},
		{"trailing", `{"ticket_id":"51"}{}`},
		{"duplicate", `{"ticket_id":"51","ticket_id":"51"}`},
		{"unknown", `{"ticket_id":"51","extra":true}`},
		{"case", `{"Ticket_id":"51"}`},
		{"null_field", `{"ticket_id":null}`},
		{"object_field", `{"ticket_id":{}}`},
		{"array_field", `{"ticket_id":[]}`},
		{"numeric", `{"ticket_id":51}`},
		{"missing", "{}"},
		{"zero", `{"ticket_id":"0"}`},
		{"leading_zero", `{"ticket_id":"051"}`},
		{"signed", `{"ticket_id":"+51"}`},
		{"space", `{"ticket_id":" 51"}`},
		{"overflow", `{"ticket_id":"18446744073709551616"}`},
		{"invalid_utf8", string([]byte{0xff})},
		{"oversize", strings.Repeat(" ", 16*1024+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			got, err := NormalizeCall(QueryTicketStatusName, tc.raw)
			toolCode(t, err, "invalid_argument")
			if got != "" {
				t.Fatal("invalid normalization returned data")
			}
			q, err := NewQueryTicketStatusTool(r.caller, r)
			toolCode(t, err, "")
			got, err = q.InvokableRun(context.Background(), tc.raw)
			toolCode(t, err, "invalid_argument")
			if got != "" {
				t.Fatal("invalid query returned data")
			}
			r.wantCalls()
		})
	}
}

func TestToolContractNormalize(t *testing.T) {
	cases := []struct{ name, tool, raw, want, code string }{
		{"search", SearchRuleName, `{"query":"  规则  "}`, `{"query":"规则","top_k":3}`, ""},
		{"current", GetRuleDetailName, `{"document_id":"21","mode":"current"}`, `{"document_id":"21","mode":"current"}`, ""},
		{"history", GetRuleDetailName, `{"mode":"history","article_id":"41","version_id":"31"}`, `{"article_id":"41","mode":"history","version_id":"31"}`, ""},
		{"eligibility", CheckAppealEligibilityName, `{"subject_type":"account","reason":" a\r\nb ","subject_ref":" x "}`, `{"subject_type":"account","subject_ref":"x","reason":"a\nb"}`, ""},
		{"create", CreateAppealTicketName, toolInput, toolInput, ""},
		{"query_max", QueryTicketStatusName, `{"ticket_id":"18446744073709551615"}`, `{"ticket_id":"18446744073709551615"}`, ""},
		{"empty_query", SearchRuleName, `{"query":" "}`, "", "invalid_argument"},
		{"query_limit", SearchRuleName, `{"query":"` + strings.Repeat("文", 201) + `"}`, "", "invalid_argument"},
		{"top_k_zero", SearchRuleName, `{"query":"规则","top_k":0}`, "", "invalid_argument"},
		{"top_k_high", SearchRuleName, `{"query":"规则","top_k":4}`, "", "invalid_argument"},
		{"top_k_fraction", SearchRuleName, `{"query":"规则","top_k":1.5}`, "", "invalid_argument"},
		{"mixed_detail", GetRuleDetailName, `{"mode":"current","document_id":"21","article_id":"41"}`, "", "invalid_argument"},
		{"missing_history", GetRuleDetailName, `{"mode":"history","version_id":"31"}`, "", "invalid_argument"},
		{"detail_mode", GetRuleDetailName, `{"mode":"other"}`, "", "invalid_argument"},
		{"subject", CheckAppealEligibilityName, `{"subject_type":"invalid"}`, "", "invalid_argument"},
		{"incomplete_create", CreateAppealTicketName, `{"subject_type":"account","intake_mode":"standard"}`, "", "needs_more_info"},
		{"mode", CreateAppealTicketName, strings.Replace(toolInput, "manual_review", "other", 1), "", "invalid_argument"},
		{"unknown_tool", "other", "{}", "", "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCall(tc.tool, tc.raw)
			toolCode(t, err, tc.code)
			if got != tc.want {
				t.Fatalf("normalized=%q; want=%q", got, tc.want)
			}
			if err == nil {
				again, e := NormalizeCall(tc.tool, got)
				toolCode(t, e, "")
				if again != got {
					t.Fatal("normalization not stable")
				}
			}
		})
	}
}

func TestToolContractQuery(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*toolRig)
	}{
		{"success", "", func(r *toolRig) {}},
		{"not_found", "not_found", func(r *toolRig) { r.err["query"] = service.ErrAppealTicketNotFound }},
		{"internal_error", "unavailable", func(r *toolRig) { r.err["query"] = errors.New("private-storage-detail") }},
		{"nil_result", "invalid_result", func(r *toolRig) { r.ticket = nil }},
		{"wrong_id", "invalid_result", func(r *toolRig) { r.ticket.TicketID++ }},
		{"wrong_status", "invalid_result", func(r *toolRig) { r.ticket.Status = "other" }},
		{"zero_created", "invalid_result", func(r *toolRig) { r.ticket.CreatedAt = time.Time{} }},
		{"old_updated", "invalid_result", func(r *toolRig) { r.ticket.UpdatedAt = toolAt.Add(-time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			tc.edit(r)
			q, e := NewQueryTicketStatusTool(r.caller, r)
			toolCode(t, e, "")
			raw, e := q.InvokableRun(context.Background(), `{"ticket_id":"51"}`)
			toolCode(t, e, tc.code)
			r.wantCalls("query")
			if e != nil {
				if raw != "" {
					t.Fatal("error returned data")
				}
				return
			}
			toolKeys(t, raw, "ticket_id", "status", "created_at", "updated_at")
			var got TicketStatusResult
			if e = json.Unmarshal([]byte(raw), &got); e != nil {
				t.Fatal(e)
			}
			if got.TicketID != "51" || got.Status != model.AppealTicketPendingReview ||
				!got.CreatedAt.Equal(toolAt) {
				t.Fatal("wrong projection")
			}
		})
	}
}

func TestToolContractSearch(t *testing.T) {
	r := newToolRig(t)
	search, e := NewSearchRuleTool(r.session, r)
	toolCode(t, e, "")
	a, e := search.Execute(context.Background(), `{"query":"  规则  ","top_k":1}`)
	toolCode(t, e, "")
	if r.query != "规则" || r.topK != 1 || len(a.Items) != 1 ||
		a.Items[0].RefID != "R1" || len(a.EvidenceHandle) != 38 {
		t.Fatal("search contract")
	}
	r.wantCalls("owner", "retrieve")

	original := r.retrieved.Items[0].Rule.ArticleContent
	r.retrieved.Items[0].Rule.ArticleContent = "changed input"
	copy, e := r.session.Evidence(a.EvidenceHandle)
	toolCode(t, e, "")
	if copy.Items[0].Rule.ArticleContent != original {
		t.Fatal("stored evidence aliases input")
	}
	copy.Items[0].Rule.ArticleContent = "changed output"
	copy.Items[0].ChunkIDs[0] = 99
	copy, e = r.session.Evidence(a.EvidenceHandle)
	toolCode(t, e, "")
	if copy.Items[0].Rule.ArticleContent != original || copy.Items[0].ChunkIDs[0] != 11 {
		t.Fatal("stored evidence aliases output")
	}

	other := newToolRig(t)
	_, e = other.session.Evidence(a.EvidenceHandle)
	toolCode(t, e, "invalid_argument")

	b, e := search.Execute(context.Background(), `{"query":"规则"}`)
	toolCode(t, e, "")
	if b.EvidenceHandle == a.EvidenceHandle || r.topK != 3 {
		t.Fatal("search did not renew handle/default")
	}
	_, e = r.session.Evidence(a.EvidenceHandle)
	toolCode(t, e, "invalid_argument")
	_, e = search.Execute(context.Background(), "{}")
	toolCode(t, e, "invalid_argument")
	_, e = r.session.Evidence(b.EvidenceHandle)
	toolCode(t, e, "invalid_argument")
}

func TestToolContractSearchOutcomes(t *testing.T) {
	cases := []struct {
		name, code  string
		edit        func(*toolRig)
		hasEvidence bool
	}{
		{"degraded", "", func(r *toolRig) {
			r.retrieved.Outcome = retrieval.HybridDegraded
			r.retrieved.Degraded = true
		}, true},
		{"empty", "", func(r *toolRig) {
			r.retrieved.Items = []retrieval.HybridItem{}
			r.retrieved.Stats.FinalCount = 0
			r.retrieved.Outcome = retrieval.HybridNoEvidence
			r.retrieved.EmptyReason = "no_candidates"
		}, false},
		{"expired", "", func(r *toolRig) {
			at := toolAt
			r.retrieved.Items[0].Rule.Chunk.ExpiresAt = &at
		}, false},
		{"bad_count", "invalid_result", func(r *toolRig) {
			r.retrieved.Stats.FinalCount = 2
		}, false},
		{"too_many", "invalid_result", func(r *toolRig) {
			r.retrieved.Items = append(r.retrieved.Items, r.retrieved.Items[0])
		}, false},
		{"retrieve_error", "unavailable", func(r *toolRig) {
			r.err["retrieve"] = errors.New("private")
		}, false},
		{"foreign_session", "not_found", func(r *toolRig) { r.conv.UserID = 8 }, false},
		{"missing_session", "not_found", func(r *toolRig) {
			r.err["owner"] = service.ErrConversationNotFound
		}, false},
		{"nil_session_result", "invalid_result", func(r *toolRig) { r.conv = nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			tc.edit(r)
			search, e := NewSearchRuleTool(r.session, r)
			toolCode(t, e, "")
			got, e := search.Execute(context.Background(), `{"query":"规则","top_k":1}`)
			toolCode(t, e, tc.code)
			if e == nil {
				if (got.EvidenceHandle != "") != tc.hasEvidence ||
					(len(got.Items) > 0) != tc.hasEvidence {
					t.Fatal("evidence availability")
				}
				if !tc.hasEvidence && got.Reason == "" {
					t.Fatal("missing reason")
				}
			} else if got != nil {
				t.Fatal("error returned result")
			}
			if strings.Contains(tc.name, "session") {
				r.wantCalls("owner")
			} else {
				r.wantCalls("owner", "retrieve")
			}
		})
	}
}

func TestToolContractDetail(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*toolRig)
	}{
		{"current", "", func(r *toolRig) {}},
		{"expired", "not_found", func(r *toolRig) {
			at := toolAt
			r.current.Version.ExpiresAt = &at
		}},
		{"future", "not_found", func(r *toolRig) {
			at := toolAt.Add(time.Second)
			r.current.Version.EffectiveAt = &at
		}},
		{"withdrawn", "not_found", func(r *toolRig) { r.current.Version.Status = model.RuleVersionStatusWithdrawn }},
		{"draft", "invalid_result", func(r *toolRig) { r.current.Version.Status = model.RuleVersionStatusDraft }},
		{"wrong_document", "invalid_result", func(r *toolRig) { r.current.Version.DocumentID++ }},
		{"wrong_article_version", "invalid_result", func(r *toolRig) { r.current.Articles[0].VersionID++ }},
		{"duplicate_article", "invalid_result", func(r *toolRig) {
			r.current.Articles = append(r.current.Articles, r.current.Articles[0])
		}},
		{"empty_articles", "invalid_result", func(r *toolRig) { r.current.Articles = nil }},
		{"blank_content", "invalid_result", func(r *toolRig) { r.current.Version.Content = " " }},
		{"too_large", "result_too_large", func(r *toolRig) {
			r.current.Version.Content = strings.Repeat("x", 32*1024)
		}},
		{"nil_detail", "invalid_result", func(r *toolRig) { r.current = nil }},
		{"read_error", "not_found", func(r *toolRig) {
			r.err["current"] = service.ErrKnowledgeCurrentVersionNotFound
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			tc.edit(r)
			d, e := NewGetRuleDetailToolWithClock(r.session, r, func() time.Time { return toolAt })
			toolCode(t, e, "")
			got, e := d.Execute(context.Background(), `{"mode":"current","document_id":"21"}`)
			toolCode(t, e, tc.code)
			r.wantCalls("owner", "current")
			if e != nil {
				if got != nil {
					t.Fatal("error returned detail")
				}
				return
			}
			if got.Usage != "current_rule_read_only" ||
				got.Content != r.current.Version.Content ||
				len(got.Articles) != 1 || got.History != nil {
				t.Fatal("current projection")
			}
			*got.EffectiveAt = time.Time{}
			if r.current.Version.EffectiveAt.IsZero() {
				t.Fatal("time pointer aliases source")
			}
		})
	}
	t.Run("history_keeps_search", func(t *testing.T) {
		r := newToolRig(t)
		s, e := NewSearchRuleTool(r.session, r)
		toolCode(t, e, "")
		found, e := s.Execute(context.Background(), `{"query":"规则"}`)
		toolCode(t, e, "")
		r.history.Version.Status = model.RuleVersionStatusWithdrawn
		r.history.State.IsWithdrawn = true
		d, e := NewGetRuleDetailToolWithClock(r.session, r, func() time.Time { return toolAt })
		toolCode(t, e, "")
		got, e := d.Execute(context.Background(), `{"mode":"history","version_id":"31","article_id":"41"}`)
		toolCode(t, e, "")
		if got.Usage != "historical_reference_only" || got.Content != "" ||
			got.History == nil || !got.History.Withdrawn || !got.History.Superseded {
			t.Fatal("history projection")
		}
		_, e = r.session.Evidence(found.EvidenceHandle)
		toolCode(t, e, "")
		r.wantCalls("owner", "retrieve", "owner", "history")
	})
}

func TestToolContractEligibility(t *testing.T) {
	for _, decision := range []model.AppealEligibilityDecision{
		model.AppealEligibilityEligible,
		model.AppealEligibilityIneligible,
		model.AppealEligibilityNeedsMoreInfo,
		model.AppealEligibilityManualReview,
	} {
		t.Run(string(decision), func(t *testing.T) {
			r := newToolRig(t)
			r.snapshot = toolSnapshot(decision)
			x, e := NewCheckAppealEligibilityTool(r.caller, r)
			toolCode(t, e, "")
			raw := `{"subject_type":"account","subject_ref":" x ","reason":" why "}`
			if decision == model.AppealEligibilityNeedsMoreInfo {
				raw = `{"subject_type":"account","subject_ref":" x "}`
			}
			got, e := x.Execute(context.Background(), raw)
			toolCode(t, e, "")
			r.wantCalls("eligibility")
			if got.Decision != decision || r.facts.SubjectRef != "x" {
				t.Fatal("eligibility projection/input")
			}
			if decision == model.AppealEligibilityNeedsMoreInfo &&
				(r.facts.Reason != "" || len(got.MissingFields) != 1) {
				t.Fatal("missing facts")
			}
			if decision == model.AppealEligibilityManualReview &&
				(got.PolicyCode != "" || len(got.EvidenceRefs) != 0) {
				t.Fatal("invented policy")
			}
			if len(got.EvidenceRefs) > 0 && got.EvidenceRefs[0].ArticleID != "41" {
				t.Fatal("evidence id")
			}
		})
	}
	t.Run("invalid_snapshot", func(t *testing.T) {
		r := newToolRig(t)
		r.snapshot.Decision = model.AppealEligibilityEligible
		x, e := NewCheckAppealEligibilityTool(r.caller, r)
		toolCode(t, e, "")
		got, e := x.Execute(context.Background(), `{"subject_type":"account"}`)
		toolCode(t, e, "invalid_result")
		if got != nil {
			t.Fatal("invalid snapshot returned result")
		}
	})
}

func TestToolContractProposal(t *testing.T) {
	cases := []struct {
		name, code string
		edit       func(*toolRig)
	}{
		{"valid", "", func(r *toolRig) {}},
		{"prepare_error", "ineligible", func(r *toolRig) { r.err["prepare"] = service.ErrAppealIneligible }},
		{"nil", "invalid_result", func(r *toolRig) { r.proposal = nil }},
		{"input", "invalid_result", func(r *toolRig) { r.proposal.Input.Reason = "different" }},
		{"binding", "invalid_result", func(r *toolRig) { r.proposal.Binding.UserID++ }},
		{"digest", "invalid_result", func(r *toolRig) { r.proposal.Binding.PolicyDigest = strings.Repeat("c", 64) }},
		{"token", "invalid_result", func(r *toolRig) { r.proposal.Token = "" }},
		{"status", "invalid_result", func(r *toolRig) { r.proposal.InitialStatus = model.AppealTicketSubmitted }},
		{"expiry", "invalid_result", func(r *toolRig) { r.proposal.ExpiresAt = r.proposal.ExpiresAt.Add(time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			tc.edit(r)
			reg := r.registry(nil)
			raw, e := reg.Invoke(context.Background(), CreateAppealTicketName, toolInput)
			toolCode(t, e, tc.code)
			r.wantCalls("owner", "prepare")
			result, effect := reg.BusinessResult()
			if result != nil || effect != "none" {
				t.Fatal("proposal wrote business result")
			}
			if e == nil {
				if raw != `{"decision":"confirmation_required","business_effect":"none"}` {
					t.Fatal("model-visible proposal")
				}
				p := reg.Proposal()
				if p == nil || p.Token != r.proposal.Token {
					t.Fatal("missing private proposal")
				}
				p.Token = "changed"
				p.Eligibility.FactSources[0].Field = "changed"
				if reg.Proposal().Token != r.proposal.Token ||
					reg.Proposal().Eligibility.FactSources[0].Field == "changed" {
					t.Fatal("proposal aliases")
				}
			} else if raw != "" || reg.Proposal() != nil {
				t.Fatal("invalid proposal retained")
			}
			_, e = reg.Invoke(context.Background(), CreateAppealTicketName, toolInput)
			toolCode(t, e, "create_already_attempted")
			r.wantCalls("owner", "prepare")
		})
	}
}

func TestToolContractConfirmed(t *testing.T) {
	cases := []struct {
		name, code, effect string
		edit               func(*toolRig)
	}{
		{"committed", "", "committed", func(r *toolRig) {}},
		{"replayed", "", "committed", func(r *toolRig) { r.result.Replayed = true }},
		{"standard", "", "committed", func(r *toolRig) {
			r.input.IntakeMode = model.AppealIntakeStandard
			r.snapshot = toolSnapshot(model.AppealEligibilityEligible)
			r.refreshProposal()
			r.result.InitialStatus = model.AppealTicketSubmitted
		}},
		{"audit_failure", "audit_unavailable", "none", func(r *toolRig) { r.err["audit"] = errors.New("private") }},
		{"precondition", "precondition_changed", "none", func(r *toolRig) { r.err["create"] = service.ErrAppealPreconditionChanged }},
		{"unknown", "outcome_unknown", "unknown", func(r *toolRig) { r.err["create"] = service.ErrAppealOutcomeUnknown }},
		{"nil_receipt", "outcome_unknown", "unknown", func(r *toolRig) { r.result = nil }},
		{"bad_status", "outcome_unknown", "unknown", func(r *toolRig) { r.result.InitialStatus = "other" }},
		{"zero_id", "outcome_unknown", "unknown", func(r *toolRig) { r.result.TicketID = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			tc.edit(r)
			reg := r.registry(r.confirmed())
			_, e := reg.Invoke(context.Background(), QueryTicketStatusName, `{"ticket_id":"51"}`)
			toolCode(t, e, "confirmation_required")
			r.wantCalls()

			raw, e := reg.InvokeConfirmed(context.Background())
			toolCode(t, e, tc.code)
			if tc.name == "audit_failure" {
				r.wantCalls("owner", "audit")
			} else {
				r.wantCalls("owner", "audit", "create")
			}
			got, effect := reg.BusinessResult()
			if effect != tc.effect {
				t.Fatalf("effect=%s", effect)
			}
			if effect == "committed" {
				if got == nil || got.TicketID != 51 || got.Replayed != r.result.Replayed {
					t.Fatal("receipt lost")
				}
				toolKeys(t, raw, "decision", "business_effect", "ticket")
				got.TicketID = 99
				again, _ := reg.BusinessResult()
				if again.TicketID != 51 {
					t.Fatal("receipt aliases")
				}
			} else if got != nil || raw != "" {
				t.Fatal("unexpected receipt")
			}
			_, e = reg.InvokeConfirmed(context.Background())
			toolCode(t, e, "create_already_attempted")
		})
	}
	t.Run("missing_audit", func(t *testing.T) {
		r := newToolRig(t)
		x, e := NewCreateAppealTicketTool(r.session, r, nil, r.confirmed())
		toolCode(t, e, "")
		_, e = x.InvokableRun(context.Background(), toolInput)
		toolCode(t, e, "audit_unavailable")
		r.wantCalls("owner")
	})
	t.Run("changed_input", func(t *testing.T) {
		r := newToolRig(t)
		x, e := NewCreateAppealTicketTool(r.session, r, r, r.confirmed())
		toolCode(t, e, "")
		_, e = x.InvokableRun(context.Background(), strings.Replace(toolInput, "test reason", "changed", 1))
		toolCode(t, e, "precondition_changed")
		r.wantCalls("owner")
	})
	t.Run("cancel_after_commit", func(t *testing.T) {
		r := newToolRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.after = func(name string) {
			if name == "create" {
				cancel()
			}
		}
		reg := r.registry(r.confirmed())
		_, e := reg.InvokeConfirmed(ctx)
		toolCode(t, e, "")
		got, effect := reg.BusinessResult()
		if got == nil || effect != "committed" {
			t.Fatal("committed fact lost on cancellation")
		}
	})
}

func TestToolContractRegistry(t *testing.T) {
	r := newToolRig(t)
	reg := r.registry(nil)
	infos, e := reg.Infos(context.Background())
	toolCode(t, e, "")
	names := []string{
		SearchRuleName, GetRuleDetailName, CheckAppealEligibilityName,
		CreateAppealTicketName, QueryTicketStatusName,
	}
	properties := [][]string{
		{"query", "top_k"},
		{"mode", "document_id", "version_id", "article_id"},
		{"subject_type", "subject_ref", "reason"},
		{"subject_type", "subject_ref", "reason", "intake_mode"},
		{"ticket_id"},
	}
	required := [][]string{
		{"query"}, {"mode"}, {"subject_type"},
		{"subject_type", "subject_ref", "reason", "intake_mode"}, {"ticket_id"},
	}
	if len(infos) != 5 {
		t.Fatal("registry must expose exactly five tools")
	}
	for i, info := range infos {
		if info == nil || info.Name != names[i] || info.Desc == "" || info.ParamsOneOf == nil {
			t.Fatal("tool metadata")
		}
		sc, e := info.ParamsOneOf.ToJSONSchema()
		if e != nil {
			t.Fatal(e)
		}
		var got struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
			Additional *bool                      `json:"additionalProperties"`
			OneOf      []json.RawMessage          `json:"oneOf"`
		}
		if e = json.Unmarshal([]byte(toolJSON(t, sc)), &got); e != nil {
			t.Fatal(e)
		}
		if got.Type != "object" || got.Additional == nil || *got.Additional {
			t.Fatal("schema object contract")
		}
		toolKeys(t, toolJSON(t, got.Properties), properties[i]...)
		sort.Strings(got.Required)
		sort.Strings(required[i])
		if !reflect.DeepEqual(got.Required, required[i]) {
			t.Fatal("required fields")
		}
		if i == 1 && len(got.OneOf) != 2 {
			t.Fatal("detail mode alternatives")
		}
	}
	_, e = reg.InvokeConfirmed(context.Background())
	toolCode(t, e, "confirmation_required")
	_, e = reg.Invoke(context.Background(), "unknown", "{}")
	toolCode(t, e, "invalid_argument")
	r.wantCalls()

	t.Run("typed_nil", func(t *testing.T) {
		var absent *toolRig
		deps := RegistryDependencies{
			Conversations: absent, Retriever: r, Knowledge: r, Appeals: r,
		}
		_, e := NewRegistry(r.caller, deps, nil)
		toolCode(t, e, "configuration_error")
	})
	t.Run("serialized", func(t *testing.T) {
		if !reg.mu.TryLock() {
			t.Fatal("unexpected locked registry")
		}
		defer reg.mu.Unlock()
		_, e := reg.Invoke(context.Background(), QueryTicketStatusName, `{"ticket_id":"51"}`)
		toolCode(t, e, "invalid_argument")
		r.wantCalls()
	})
}

func TestToolContractCancellation(t *testing.T) {
	names := []string{
		SearchRuleName, GetRuleDetailName, CheckAppealEligibilityName,
		CreateAppealTicketName, QueryTicketStatusName,
	}
	args := []string{
		`{"query":"规则"}`,
		`{"mode":"current","document_id":"21"}`,
		`{"subject_type":"account"}`,
		toolInput,
		`{"ticket_id":"51"}`,
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			r := newToolRig(t)
			reg := r.registry(nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			raw, e := reg.Invoke(ctx, name, args[i])
			toolCode(t, e, "cancelled")
			if raw != "" {
				t.Fatal("cancelled call returned data")
			}
			r.wantCalls()
		})
	}
	t.Run("during_query", func(t *testing.T) {
		r := newToolRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.after = func(name string) {
			if name == "query" {
				cancel()
			}
		}
		raw, e := r.registry(nil).Invoke(ctx, QueryTicketStatusName, `{"ticket_id":"51"}`)
		toolCode(t, e, "cancelled")
		if raw != "" {
			t.Fatal("late cancellation returned data")
		}
	})
	t.Run("info", func(t *testing.T) {
		r := newToolRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, e := r.registry(nil).Infos(ctx)
		toolCode(t, e, "cancelled")
		r.wantCalls()
	})
}

func TestToolContractExecutionValidation(t *testing.T) {
	cases := []struct{ name, tool, raw string }{
		{"search", SearchRuleName, `{"query":"规则","extra":true}`},
		{"detail", GetRuleDetailName, `{"mode":"current","document_id":"21","version_id":"31"}`},
		{"eligibility", CheckAppealEligibilityName, `{"subject_type":"account","extra":true}`},
		{"create", CreateAppealTicketName, `{"subject_type":"account","subject_ref":"x","reason":"why","intake_mode":"invalid"}`},
		{"query", QueryTicketStatusName, `{"ticket_id":"051"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newToolRig(t)
			raw, e := r.registry(nil).Invoke(context.Background(), tc.tool, tc.raw)
			toolCode(t, e, "invalid_argument")
			if raw != "" {
				t.Fatal("invalid input returned data")
			}
			r.wantCalls()
		})
	}
	t.Run("caller", func(t *testing.T) {
		if _, e := NewCaller(0, 9); !errors.Is(e, service.ErrUnauthorized) {
			t.Fatal(e)
		}
		if _, e := NewCaller(7, 0); !errors.Is(e, service.ErrInvalidArgument) {
			t.Fatal(e)
		}
	})
	t.Run("result_budget", func(t *testing.T) {
		raw, e := encodeResult(strings.Repeat("x", 32*1024))
		toolCode(t, e, "result_too_large")
		if raw != "" {
			t.Fatal("oversize result was truncated or returned")
		}
	})
}
