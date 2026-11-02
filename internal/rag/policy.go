package rag

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/retrieval"
)

const MaxChatQuestionRunes = 10000

var (
	ErrInvalidPreparation = errors.New("invalid RAG preparation")
	ErrInvalidHistory     = errors.New("invalid RAG history")
)

type FixedReason string

const (
	FixedNone             FixedReason = ""
	FixedQuestionTooLong  FixedReason = "question_too_long"
	FixedContextTooLarge  FixedReason = "context_too_large"
	FixedNoEvidence       FixedReason = "no_evidence"
	FixedDegradedEmpty    FixedReason = "degraded_empty"
	FixedUnusableEvidence FixedReason = "unusable_evidence"
)

func PrepareQuestion(ctx context.Context, raw string) (query string, reason FixedReason, err error) {
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			query = ""
			reason = FixedNone
			err = contextErr
		}
	}()

	if err := ctx.Err(); err != nil {
		return "", FixedNone, err
	}
	if !utf8.ValidString(raw) {
		return "", FixedNone, llm.ErrInvalidInput
	}

	question := strings.TrimSpace(raw)
	count := utf8.RuneCountInString(question)

	if count == 0 || count > MaxChatQuestionRunes {
		return "", FixedNone, llm.ErrInvalidInput
	}
	if count > retrieval.MaxQueryRunes {
		return "", FixedQuestionTooLong, nil
	}

	return question, FixedNone, nil
}

func FixedText(reason FixedReason) (string, error) {
	switch reason {
	case FixedQuestionTooLong:
		return "请将问题提炼为200字以内，并说明适用对象或具体场景。", nil
	case FixedContextTooLarge:
		return "相关规则的完整内容超出本次处理范围。请缩小问题范围，例如明确规则名称、条款或适用场景。", nil
	case FixedNoEvidence:
		return "当前知识库未找到足够依据回答这个问题。请补充规则名称或具体场景；这不表示平台没有相关规则。", nil
	case FixedDegradedEmpty:
		return "当前部分检索服务不可用，暂未找到可核实的依据。请稍后重试，或补充规则名称与具体场景。", nil
	case FixedUnusableEvidence:
		return "当前检索资料无法形成可核实的依据，暂不能据此回答。请补充规则名称或具体场景。", nil
	default:
		return "", ErrInvalidPreparation
	}
}

func prepareRetrievedEvidence(ctx context.Context, result retrieval.HybridResult) (EvidenceSet, FixedReason, error) {
	count := len(result.Items)

	if result.Items == nil || count > ChatTopK || result.Stats.FinalCount != count || result.ErrorKind != "" {
		return EvidenceSet{}, FixedNone, ErrInvalidEvidence
	}

	valid := false

	switch result.Outcome {
	case retrieval.HybridSuccess, retrieval.HybridDegraded:
		valid = count > 0 && result.EmptyReason == "" && result.Degraded == (result.Outcome == retrieval.HybridDegraded)

	case retrieval.HybridNoEvidence, retrieval.HybridDegradedEmpty:
		valid = count == 0 && (result.EmptyReason == "no_candidates" || result.EmptyReason == "current_filter") && result.Degraded == (result.Outcome == retrieval.HybridDegradedEmpty)

	case retrieval.HybridNoUsableEvidence:
		valid = count == 0 && result.EmptyReason == "conflicts"
	}

	if !valid {
		return EvidenceSet{}, FixedNone, ErrInvalidEvidence
	}

	evidence, err := BuildEvidence(ctx, result.Items, result.RetrievedAt)
	if err != nil {
		return EvidenceSet{}, FixedNone, err
	}

	if evidence.stats.AcceptedCount > 0 {
		return evidence, FixedNone, nil
	}

	if count > 0 || result.Outcome == retrieval.HybridNoUsableEvidence {
		return evidence, FixedUnusableEvidence, nil
	}
	if result.Outcome == retrieval.HybridDegradedEmpty {
		return evidence, FixedDegradedEmpty, nil
	}

	return evidence, FixedNoEvidence, nil
}
