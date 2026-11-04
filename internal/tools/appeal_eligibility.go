package tools

import (
	"context"
	"strconv"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

const CheckAppealEligibilityName = "check_appeal_eligibility"

type EligibilityReader interface {
	CheckEligibility(context.Context, uint64, uint64, service.AppealFacts) (model.AppealEligibilitySnapshot, error)
}

type CheckAppealEligibilityTool struct {
	caller Caller
	reader EligibilityReader
}

var _ tool.InvokableTool = (*CheckAppealEligibilityTool)(nil)
var _ EligibilityReader = (*service.AppealTicketService)(nil)

func NewCheckAppealEligibilityTool(caller Caller, reader EligibilityReader) (*CheckAppealEligibilityTool, error) {
	if err := caller.validate(); err != nil {
		return nil, fail(err)
	}
	if reader == nil {
		return nil, fail(ErrConfiguration)
	}
	return &CheckAppealEligibilityTool{
		caller: caller,
		reader: reader,
	}, nil
}

type EligibilityEvidence struct {
	DocumentID          string `json:"document_id"`
	VersionID           string `json:"version_id"`
	ArticleID           string `json:"article_id"`
	PublicationRevision string `json:"publication_revision"`
}

type EligibilityResult struct {
	Decision       model.AppealEligibilityDecision `json:"decision"`
	ReasonCode     string                          `json:"reason_code"`
	MissingFields  []string                        `json:"missing_fields"`
	CheckedAt      time.Time                       `json:"checked_at"`
	PolicyCode     string                          `json:"policy_code"`
	PolicyRevision string                          `json:"policy_revision"`
	EvidenceRefs   []EligibilityEvidence           `json:"evidence_refs"`
	FactSources    []model.AppealFactProvenance    `json:"fact_sources"`
}

func (t *CheckAppealEligibilityTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return ruleInfo(
		ctx,
		CheckAppealEligibilityName,
		"预检查申诉资格，返回四态和事实来源。"+
			"缺少可靠政策时人工复核；不代表平台受理，也不创建工单。",
		`{
			"type":"object",
			"properties":{
				"subject_type":{
					"type":"string",
					"enum":["account","content","other"]
				},
				"subject_ref":{"type":"string","maxLength":128},
				"reason":{"type":"string","maxLength":2000}
			},
			"required":["subject_type"],
			"additionalProperties":false
		}`,
	)
}

func (t *CheckAppealEligibilityTool) Execute(ctx context.Context, raw string) (*EligibilityResult, error) {
	if t == nil || t.reader == nil {
		return nil, fail(ErrConfiguration)
	}
	if err := t.caller.validate(); err != nil {
		return nil, fail(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}

	var facts service.AppealFacts
	if err := decodeObject(raw, &facts, "subject_type", "subject_ref", "reason"); err != nil {
		return nil, fail(err)
	}

	facts, _, err := service.NormalizeAppealFacts(facts)
	if err != nil {
		return nil, fail(err)
	}

	work, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	snapshot, err := t.reader.CheckEligibility(work, t.caller.userID, t.caller.conversationID, facts)
	if err != nil {
		return nil, fail(err)
	}

	snapshot, err = service.NormalizeAppealSnapshot(snapshot)
	if err != nil {
		return nil, fail(ErrInvalidResult)
	}

	result := &EligibilityResult{
		Decision:       snapshot.Decision,
		ReasonCode:     snapshot.ReasonCode,
		MissingFields:  snapshot.MissingFields,
		CheckedAt:      snapshot.CheckedAt,
		PolicyCode:     snapshot.PolicyCode,
		PolicyRevision: snapshot.PolicyRevision,
		EvidenceRefs: make(
			[]EligibilityEvidence, 0, len(snapshot.EvidenceRefs),
		),
		FactSources: snapshot.FactSources,
	}

	for _, ref := range snapshot.EvidenceRefs {
		result.EvidenceRefs = append(
			result.EvidenceRefs,
			EligibilityEvidence{
				DocumentID: strconv.FormatUint(ref.DocumentID, 10),
				VersionID:  strconv.FormatUint(ref.VersionID, 10),
				ArticleID:  strconv.FormatUint(ref.ArticleID, 10),
				PublicationRevision: strconv.FormatUint(
					ref.PublicationRevision, 10,
				),
			},
		)
	}

	if err := work.Err(); err != nil {
		return nil, fail(err)
	}
	if _, err := encodeResult(result); err != nil {
		return nil, err
	}
	return result, nil
}

func (t *CheckAppealEligibilityTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	result, err := t.Execute(ctx, raw)
	if err != nil {
		return "", err
	}
	return encodeResult(result)
}
