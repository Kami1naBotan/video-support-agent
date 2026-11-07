package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/model"
)

var DomainAt = time.Date(2026, 11, 3, 8, 0, 0, 123000000, time.UTC)

func DomainInput() CreateAppealInput {
	return CreateAppealInput{
		SubjectType: model.AppealSubjectAccount, SubjectRef: "user-42",
		Reason: "Please review.\nLine 2", IntakeMode: model.AppealIntakeManualReview,
	}
}

func DomainSnapshot() model.AppealEligibilitySnapshot {
	return model.AppealEligibilitySnapshot{
		SchemaVersion: 1, Decision: model.AppealEligibilityEligible,
		ReasonCode: "test_only_eligible", CheckedAt: DomainAt,
		PolicyCode: "TEST_ONLY", PolicyRevision: "v1",
		EvidenceRefs: []model.AppealRuleEvidence{
			{DocumentID: 2, VersionID: 20, ArticleID: 200, PublicationRevision: 1},
			{DocumentID: 1, VersionID: 10, ArticleID: 100, PublicationRevision: 1},
		},
		FactSources: []model.AppealFactProvenance{
			{Field: "subject_type", Source: model.AppealFactUserReported},
			{Field: "subject_ref", Source: model.AppealFactUserReported},
			{Field: "reason", Source: model.AppealFactUserReported},
		},
	}
}

func TestDomainInput(t *testing.T) {
	t.Run("normalization", func(t *testing.T) {
		raw := DomainInput()
		raw.SubjectRef = " \tuser-42\u3000"
		raw.Reason = "\r\nPlease review.\r\nLine 2 \t"
		got, err := NormalizeCreateAppealInput(raw)
		if err != nil || got != DomainInput() {
			t.Fatalf("normalized=%+v err=%v", got, err)
		}
		again, err := NormalizeCreateAppealInput(got)
		if err != nil || again != got {
			t.Fatalf("normalization is not idempotent: %v", err)
		}
		raw.SubjectRef, raw.Reason = "MiXeD  Ref", "A  B\nC"
		got, err = NormalizeCreateAppealInput(raw)
		if err != nil || got.SubjectRef != raw.SubjectRef || got.Reason != raw.Reason {
			t.Fatalf("internal whitespace/case changed: %+v %v", got, err)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*CreateAppealInput)
		want error
	}{
		{"subject_type", func(v *CreateAppealInput) { v.SubjectType = "ACCOUNT" }, ErrInvalidArgument},
		{"intake_mode", func(v *CreateAppealInput) { v.IntakeMode = "auto" }, ErrInvalidArgument},
		{"empty_ref", func(v *CreateAppealInput) { v.SubjectRef = " \t" }, ErrAppealNeedsMoreInfo},
		{"empty_reason", func(v *CreateAppealInput) { v.Reason = "\r\n" }, ErrAppealNeedsMoreInfo},
		{"ref_limit", func(v *CreateAppealInput) { v.SubjectRef = strings.Repeat("界", 128) }, nil},
		{"ref_over", func(v *CreateAppealInput) { v.SubjectRef = strings.Repeat("界", 129) }, ErrInvalidArgument},
		{"reason_limit", func(v *CreateAppealInput) { v.Reason = strings.Repeat("界", 2000) }, nil},
		{"reason_over", func(v *CreateAppealInput) { v.Reason = strings.Repeat("界", 2001) }, ErrInvalidArgument},
		{"ref_utf8", func(v *CreateAppealInput) { v.SubjectRef = string([]byte{0xff}) }, ErrInvalidArgument},
		{"reason_utf8", func(v *CreateAppealInput) { v.Reason = string([]byte{0xff}) }, ErrInvalidArgument},
		{"ref_nul", func(v *CreateAppealInput) { v.SubjectRef = "a\x00b" }, ErrInvalidArgument},
		{"reason_nul", func(v *CreateAppealInput) { v.Reason = "a\x00b" }, ErrInvalidArgument},
		{"raw_ref_limit", func(v *CreateAppealInput) { v.SubjectRef = strings.Repeat(" ", 16385) }, ErrInvalidArgument},
		{"raw_reason_limit", func(v *CreateAppealInput) { v.Reason = strings.Repeat(" ", 16385) }, ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := DomainInput()
			tc.edit(&v)
			_, err := NormalizeCreateAppealInput(v)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
		})
	}
	t.Run("check_can_report_missing", func(t *testing.T) {
		_, missing, err := NormalizeAppealFacts(
			AppealFacts{SubjectType: model.AppealSubjectAccount},
		)
		if err != nil || !reflect.DeepEqual(missing, []string{"subject_ref", "reason"}) {
			t.Fatalf("missing=%v err=%v", missing, err)
		}
	})
	for _, subject := range []model.AppealSubjectType{
		model.AppealSubjectAccount,
		model.AppealSubjectContent,
		model.AppealSubjectOther,
	} {
		t.Run(string(subject), func(t *testing.T) {
			v := DomainInput()
			v.SubjectType = subject
			if _, err := NormalizeCreateAppealInput(v); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDomainHash(t *testing.T) {
	const golden = "372d95aef4629aeefa284fca7d94354f2d477d23b7b618147aaa9dc2f0c4663b"
	// 独立固定JSON字节的SHA256金样本；不调用生产编码器生成期望值。
	base := DomainInput()
	got, err := AppealRequestHash(7, base)
	if err != nil || got != golden {
		t.Fatalf("hash=%s err=%v", got, err)
	}
	raw := base
	raw.SubjectRef, raw.Reason = " user-42 ", " Please review.\r\nLine 2 "
	same, err := AppealRequestHash(7, raw)
	if err != nil || same != golden {
		t.Fatalf("equivalent input: %s %v", same, err)
	}
	for _, tc := range []struct {
		name         string
		conversation uint64
		edit         func(*CreateAppealInput)
	}{
		{"conversation", 8, func(*CreateAppealInput) {}},
		{"subject", 7, func(v *CreateAppealInput) { v.SubjectType = model.AppealSubjectContent }},
		{"ref_case", 7, func(v *CreateAppealInput) { v.SubjectRef = "User-42" }},
		{"reason_spacing", 7, func(v *CreateAppealInput) { v.Reason = "Please  review.\nLine 2" }},
		{"mode", 7, func(v *CreateAppealInput) { v.IntakeMode = model.AppealIntakeStandard }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := base
			tc.edit(&v)
			changed, err := AppealRequestHash(tc.conversation, v)
			if err != nil || changed == golden {
				t.Fatalf("hash unchanged/error: %s %v", changed, err)
			}
		})
	}
	if _, err := AppealRequestHash(0, base); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero conversation: %v", err)
	}
}

func TestDomainKey(t *testing.T) {
	const key = "12345678-1234-4234-8234-123456789abc"
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"valid", key, true},
		{"empty", "", false},
		{"uppercase", strings.ToUpper(key), false},
		{"spaces", " " + key, false},
		{"version", "12345678-1234-1234-8234-123456789abc", false},
		{"variant", "12345678-1234-4234-7234-123456789abc", false},
		{"suffix", key + "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAppealOperationKey(tc.value)
			if tc.valid && err != nil ||
				!tc.valid && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error=%v valid=%v", err, tc.valid)
			}
		})
	}
	for _, value := range []string{
		"", strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("A", 64), strings.Repeat("g", 64),
	} {
		if err := ValidateAppealDigest(value); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid digest accepted: %q", value)
		}
	}
	if err := ValidateAppealDigest(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestDomainSnapshot(t *testing.T) {
	t.Run("copy_sort_time", func(t *testing.T) {
		raw := DomainSnapshot()
		raw.CheckedAt = DomainAt.
			In(time.FixedZone("test", 8*3600)).
			Add(999 * time.Microsecond)
		got, err := NormalizeAppealSnapshot(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !got.CheckedAt.Equal(DomainAt) ||
			got.CheckedAt.Location() != time.UTC ||
			got.EvidenceRefs[0].DocumentID != 1 ||
			got.FactSources[0].Field != "reason" ||
			raw.EvidenceRefs[0].DocumentID != 2 ||
			raw.FactSources[0].Field != "subject_type" ||
			got.MissingFields == nil {
			t.Fatalf("normalization or input mutation: %+v raw=%+v", got, raw)
		}
		got.EvidenceRefs[0].ArticleID = 999
		got.FactSources[0].Field = "changed"
		if raw.EvidenceRefs[1].ArticleID != 100 ||
			raw.FactSources[2].Field != "reason" {
			t.Fatal("slice aliases input")
		}
	})
	t.Run("missing_slice_copy", func(t *testing.T) {
		raw := model.AppealEligibilitySnapshot{
			SchemaVersion: 1,
			Decision:      model.AppealEligibilityNeedsMoreInfo,
			ReasonCode:    "missing_fields",
			CheckedAt:     DomainAt,
			MissingFields: []string{"subject_ref", "reason"},
			FactSources: []model.AppealFactProvenance{
				{Field: "subject_type", Source: model.AppealFactUserReported},
			},
		}
		got, err := NormalizeAppealSnapshot(raw)
		if err != nil || got.MissingFields[0] != "reason" {
			t.Fatalf("%+v %v", got, err)
		}
		got.MissingFields[0] = "changed"
		if raw.MissingFields[0] != "subject_ref" ||
			raw.MissingFields[1] != "reason" {
			t.Fatal("missing fields mutated")
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*model.AppealEligibilitySnapshot)
	}{
		{"schema", func(s *model.AppealEligibilitySnapshot) { s.SchemaVersion = 2 }},
		{"time", func(s *model.AppealEligibilitySnapshot) { s.CheckedAt = time.Time{} }},
		{"reason_code", func(s *model.AppealEligibilitySnapshot) { s.ReasonCode = "Bad Code" }},
		{"decision", func(s *model.AppealEligibilitySnapshot) { s.Decision = "approved" }},
		{"policy_revision", func(s *model.AppealEligibilitySnapshot) { s.PolicyRevision = "" }},
		{"no_evidence", func(s *model.AppealEligibilitySnapshot) { s.EvidenceRefs = nil }},
		{"zero_identity", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs[0].PublicationRevision = 0
		}},
		{"duplicate_article", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs[1] = s.EvidenceRefs[0]
		}},
		{"conflicting_document", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs[1].DocumentID = 2
		}},
		{"too_many_refs", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs = make([]model.AppealRuleEvidence, 9)
		}},
		{"source_missing", func(s *model.AppealEligibilitySnapshot) {
			s.FactSources = s.FactSources[:2]
		}},
		{"source_duplicate", func(s *model.AppealEligibilitySnapshot) {
			s.FactSources[1] = s.FactSources[0]
		}},
		{"source_invalid", func(s *model.AppealEligibilitySnapshot) {
			s.FactSources[0].Source = "model_verified"
		}},
		{"unexpected_missing", func(s *model.AppealEligibilitySnapshot) {
			s.MissingFields = []string{"reason"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DomainSnapshot()
			tc.edit(&s)
			if _, err := NormalizeAppealSnapshot(s); !errors.Is(err, ErrAppealInvalidSnapshot) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestDomainPolicyDigest(t *testing.T) {
	base := DomainSnapshot()
	want, err := AppealPolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := DomainSnapshot()
	reordered.CheckedAt = DomainAt.Add(time.Hour)
	reordered.EvidenceRefs[0], reordered.EvidenceRefs[1] =
		reordered.EvidenceRefs[1], reordered.EvidenceRefs[0]
	reordered.FactSources[0], reordered.FactSources[2] =
		reordered.FactSources[2], reordered.FactSources[0]
	got, err := AppealPolicyDigest(reordered)
	if err != nil || got != want {
		t.Fatalf("unstable digest: %s %v", got, err)
	}
	for _, tc := range []struct {
		name string
		edit func(*model.AppealEligibilitySnapshot)
	}{
		{"decision", func(s *model.AppealEligibilitySnapshot) {
			s.Decision = model.AppealEligibilityIneligible
		}},
		{"reason", func(s *model.AppealEligibilitySnapshot) {
			s.ReasonCode = "another_reason"
		}},
		{"policy_code", func(s *model.AppealEligibilitySnapshot) {
			s.PolicyCode = "TEST_OTHER"
		}},
		{"policy_revision", func(s *model.AppealEligibilitySnapshot) {
			s.PolicyRevision = "v2"
		}},
		{"publication_revision", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs[0].PublicationRevision++
		}},
		{"article", func(s *model.AppealEligibilitySnapshot) {
			s.EvidenceRefs[0].ArticleID++
		}},
		{"fact_source", func(s *model.AppealEligibilitySnapshot) {
			s.FactSources[0].Source = model.AppealFactServerVerified
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DomainSnapshot()
			tc.edit(&s)
			got, err := AppealPolicyDigest(s)
			if err != nil || got == want {
				t.Fatalf("change not bound: %s %v", got, err)
			}
		})
	}
}

type DomainProvider struct {
	policy AppealPolicy
	err    error
}

func (p DomainProvider) Resolve(
	context.Context, model.AppealSubjectType,
) (AppealPolicy, error) {
	return p.policy, p.err
}

type DomainPolicy struct {
	def    AppealPolicyDefinition
	result AppealPolicyEvaluation
	err    error
	calls  int
}

func (p *DomainPolicy) Definition() AppealPolicyDefinition {
	return p.def
}

func (p *DomainPolicy) Evaluate(
	context.Context, AppealFacts, time.Time,
) (AppealPolicyEvaluation, error) {
	p.calls++
	return p.result, p.err
}

type DomainEvidence struct {
	current bool
	err     error
	calls   int
}

func (e *DomainEvidence) CheckAppealEvidence(
	context.Context, []model.AppealRuleEvidence, time.Time,
) (bool, error) {
	e.calls++
	return e.current, e.err
}

func TestDomainEligibility(t *testing.T) {
	dependencyErr := errors.New("test dependency unavailable")
	for _, name := range []string{
		"eligible", "ineligible", "manual", "missing", "no_policy", "scope",
		"stale_evidence", "unverified", "provider_error", "evidence_error", "policy_error",
		"bad_definition", "bad_required", "bad_decision", "bad_reason", "bad_verified",
		"canceled", "nil_evidence",
	} {
		t.Run(name, func(t *testing.T) {
			policy := &DomainPolicy{
				def: AppealPolicyDefinition{
					Code: "TEST_ONLY", Revision: "v1",
					SubjectType:            model.AppealSubjectAccount,
					EvidenceRefs:           DomainSnapshot().EvidenceRefs,
					RequiredVerifiedFields: []string{"subject_ref"},
				},
				result: AppealPolicyEvaluation{
					Decision:       model.AppealEligibilityEligible,
					ReasonCode:     "test_only_eligible",
					VerifiedFields: []string{"subject_ref"},
				},
			}
			provider := DomainProvider{policy: policy}
			evidence := &DomainEvidence{current: true}
			var checker AppealEvidenceChecker = evidence
			facts := appealFactsFromInput(DomainInput())
			ctx := context.Background()
			want, reason := model.AppealEligibilityEligible, "test_only_eligible"
			var wantErr error
			wantEvidence, wantPolicy := 1, 1

			switch name {
			case "ineligible":
				policy.result.Decision = model.AppealEligibilityIneligible
				want = model.AppealEligibilityIneligible
			case "manual":
				policy.result.Decision = model.AppealEligibilityManualReview
				want = model.AppealEligibilityManualReview
			case "missing":
				facts.Reason = ""
				want, reason = model.AppealEligibilityNeedsMoreInfo, "missing_fields"
				wantEvidence, wantPolicy = 0, 0
			case "no_policy":
				provider.policy = nil
				want, reason = model.AppealEligibilityManualReview, "policy_unavailable"
				wantEvidence, wantPolicy = 0, 0
			case "scope":
				policy.def.SubjectType = model.AppealSubjectContent
				want, reason = model.AppealEligibilityManualReview, "policy_scope_mismatch"
				wantEvidence, wantPolicy = 0, 0
			case "stale_evidence":
				evidence.current = false
				want, reason = model.AppealEligibilityManualReview, "policy_evidence_unavailable"
				wantPolicy = 0
			case "unverified":
				policy.result.VerifiedFields = nil
				want, reason = model.AppealEligibilityManualReview, "facts_unverified"
			case "provider_error":
				provider.err = dependencyErr
				wantErr = dependencyErr
				wantEvidence, wantPolicy = 0, 0
			case "evidence_error":
				evidence.err = dependencyErr
				wantErr = dependencyErr
				wantPolicy = 0
			case "policy_error":
				policy.err = dependencyErr
				wantErr = dependencyErr
			case "bad_definition":
				policy.def.Revision = ""
				wantErr = ErrAppealInvalidPolicy
				wantEvidence, wantPolicy = 0, 0
			case "bad_required":
				policy.def.RequiredVerifiedFields = []string{"platform_secret"}
				wantErr = ErrAppealInvalidPolicy
				wantEvidence, wantPolicy = 0, 0
			case "bad_decision":
				policy.result.Decision = "approved"
				wantErr = ErrAppealInvalidPolicy
			case "bad_reason":
				policy.result.ReasonCode = "Bad Code"
				wantErr = ErrAppealInvalidPolicy
			case "bad_verified":
				policy.result.VerifiedFields = []string{"subject_ref", "subject_ref"}
				wantErr = ErrAppealInvalidPolicy
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				wantErr = context.Canceled
				wantEvidence, wantPolicy = 0, 0
			case "nil_evidence":
				checker = nil
				wantErr = ErrAppealConfiguration
				wantEvidence, wantPolicy = 0, 0
			}

			svc, err := NewAppealEligibilityService(provider)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Evaluate(ctx, facts, DomainAt, checker)
			if !errors.Is(err, wantErr) {
				t.Fatalf("error=%v want=%v", err, wantErr)
			}
			if evidence.calls != wantEvidence || policy.calls != wantPolicy {
				t.Fatalf("dependency calls evidence=%d policy=%d",
					evidence.calls, policy.calls)
			}
			if wantErr != nil {
				if got.Decision != "" {
					t.Fatalf("dependency error became decision: %+v", got)
				}
				return
			}
			if got.Decision != want || got.ReasonCode != reason ||
				!got.CheckedAt.Equal(DomainAt) {
				t.Fatalf("decision=%+v want=%s/%s", got, want, reason)
			}
			if _, err := NormalizeAppealSnapshot(got); err != nil {
				t.Fatalf("invalid output: %v", err)
			}
			for _, fact := range got.FactSources {
				expected := model.AppealFactUserReported
				if wantPolicy == 1 && name != "unverified" &&
					fact.Field == "subject_ref" {
					expected = model.AppealFactServerVerified
				}
				if fact.Source != expected {
					t.Fatalf("fact provenance=%+v want=%s", fact, expected)
				}
			}
			if name == "missing" &&
				!reflect.DeepEqual(got.MissingFields, []string{"reason"}) {
				t.Fatalf("missing=%v", got.MissingFields)
			}
			if name == "no_policy" &&
				(got.PolicyCode != "" || len(got.EvidenceRefs) != 0) {
				t.Fatal("invented policy")
			}
		})
	}
	t.Run("production_empty_registry", func(t *testing.T) {
		svc, err := NewAppealEligibilityService(NoAppealPolicies{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Evaluate(
			context.Background(),
			appealFactsFromInput(DomainInput()),
			DomainAt,
			nil,
		)
		if err != nil || got.Decision != model.AppealEligibilityManualReview ||
			len(got.EvidenceRefs) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	})
	if _, err := NewAppealEligibilityService(nil); !errors.Is(err, ErrAppealConfiguration) {
		t.Fatalf("nil provider: %v", err)
	}
}

func TestDomainIntake(t *testing.T) {
	for _, decision := range []model.AppealEligibilityDecision{
		model.AppealEligibilityEligible,
		model.AppealEligibilityIneligible,
		model.AppealEligibilityManualReview,
		model.AppealEligibilityNeedsMoreInfo,
	} {
		for _, mode := range []model.AppealIntakeMode{
			model.AppealIntakeStandard,
			model.AppealIntakeManualReview,
		} {
			t.Run(string(decision)+"/"+string(mode), func(t *testing.T) {
				s := DomainSnapshot()
				s.Decision = decision
				var wantStatus model.AppealTicketStatus
				var wantErr error
				switch decision {
				case model.AppealEligibilityEligible:
					if mode == model.AppealIntakeStandard {
						wantStatus = model.AppealTicketSubmitted
					} else {
						wantErr = ErrAppealPreconditionChanged
					}
				case model.AppealEligibilityManualReview:
					if mode == model.AppealIntakeManualReview {
						wantStatus = model.AppealTicketPendingReview
					} else {
						wantErr = ErrAppealPreconditionChanged
					}
				case model.AppealEligibilityIneligible:
					wantErr = ErrAppealIneligible
				case model.AppealEligibilityNeedsMoreInfo:
					s.MissingFields = []string{"reason"}
					s.FactSources = s.FactSources[:2]
					wantErr = ErrAppealNeedsMoreInfo
				}
				got, err := AppealInitialStatus(s, mode)
				if got != wantStatus || !errors.Is(err, wantErr) {
					t.Fatalf("status=%s err=%v want=%s/%v",
						got, err, wantStatus, wantErr)
				}
			})
		}
	}
	if _, err := AppealInitialStatus(DomainSnapshot(), "auto"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid mode: %v", err)
	}
}
