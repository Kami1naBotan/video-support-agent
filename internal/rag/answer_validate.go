package rag

import (
	"context"
	"strconv"
	"strings"
	"unicode"

	"video-support-agent/internal/model"
)

type checkedStatement struct {
	text   string
	refIDs []string
}

type checkedAnswer struct {
	decision      model.ReplyDecision
	statements    []checkedStatement
	scopeMismatch bool
}

func decodeAnswer(ctx context.Context, raw []byte, evidence EvidenceSet) (checkedAnswer, error) {
	object, err := answerObject(raw, "decision", "statements")
	if err != nil {
		return checkedAnswer{}, err
	}

	decisionText, err := answerString(object["decision"], 32)
	if err != nil {
		return checkedAnswer{}, err
	}
	scopeMismatch := decisionText == "scope_mismatch"
	if scopeMismatch {
		decisionText = string(model.ReplyInsufficient)
	}
	decision := model.ReplyDecision(decisionText)

	rows, err := answerArray(object["statements"], MaxAnswerStatements)
	if err != nil {
		return checkedAnswer{}, err
	}

	switch decision {
	case model.ReplyClarify, model.ReplyHandoff, model.ReplyInsufficient:
		if len(rows) != 0 {
			return checkedAnswer{}, ErrInvalidAnswer
		}
		return checkedAnswer{
			decision:      decision,
			scopeMismatch: scopeMismatch,
		}, nil

	case model.ReplyAnswer:
		if len(rows) == 0 {
			return checkedAnswer{}, ErrInvalidAnswer
		}

	default:
		return checkedAnswer{}, ErrInvalidAnswer
	}

	byRef := make(map[string]EvidenceRule, len(evidence.rules))
	for i, rule := range evidence.rules {
		expected := "R" + strconv.Itoa(i+1)
		if rule.Citation.RefID != expected {
			return checkedAnswer{}, ErrInvalidEvidence
		}
		byRef[rule.Citation.RefID] = rule
	}

	result := checkedAnswer{
		decision:   decision,
		statements: make([]checkedStatement, 0, len(rows)),
	}

	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return checkedAnswer{}, err
		}

		statement, err := answerObject(row, "text", "evidence")
		if err != nil {
			return checkedAnswer{}, err
		}

		text, err := answerString(statement["text"], MaxStatementRunes)
		if err != nil {
			return checkedAnswer{}, err
		}
		if !validStatementText(text) {
			return checkedAnswer{}, ErrInvalidAnswer
		}

		proofs, err := answerArray(statement["evidence"], MaxStatementEvidence)
		if err != nil {
			return checkedAnswer{}, err
		}
		if len(proofs) == 0 {
			return checkedAnswer{}, ErrInvalidAnswer
		}

		refs := make([]string, 0, len(proofs))
		seen := make(map[string]bool, len(proofs))

		for _, proof := range proofs {
			if err := ctx.Err(); err != nil {
				return checkedAnswer{}, err
			}

			fields, err := answerObject(proof, "ref_id", "quote")
			if err != nil {
				return checkedAnswer{}, err
			}

			refID, err := answerString(fields["ref_id"], 16)
			if err != nil {
				return checkedAnswer{}, err
			}
			quote, err := answerString(fields["quote"], MaxQuoteRunes)
			if err != nil {
				return checkedAnswer{}, err
			}

			rule, exists := byRef[refID]
			if !exists || seen[refID] {
				return checkedAnswer{}, ErrInvalidAnswer
			}
			if !strings.Contains(rule.ArticleContent, quote) {
				return checkedAnswer{}, ErrInvalidAnswer
			}

			seen[refID] = true
			refs = append(refs, refID)
		}

		result.statements = append(result.statements, checkedStatement{
			text:   strings.TrimSpace(text),
			refIDs: refs,
		})
	}

	return result, nil
}

func validStatementText(text string) bool {
	if strings.ContainsAny(text, "[]") || strings.Contains(text, "://") || strings.Contains(strings.ToLower(text), "www.") {
		return false
	}

	for _, character := range text {
		if unicode.IsControl(character) {
			return false
		}
	}

	return true
}
