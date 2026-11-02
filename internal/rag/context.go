package rag

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/retrieval"

	"github.com/cloudwego/eino/schema"
)

const (
	MaxRulesJSONBytes = 24 << 10
	MaxInputTextBytes = 32 << 10
)

type ContextStats struct {
	Evidence EvidenceStats

	ContextCount        int
	RulesBytes          int
	CurrentMessageBytes int
	InputTextBytes      int

	HistoryEvaluated       bool
	HistoryInputMessages   int
	HistoryKeptMessages    int
	HistoryDroppedMessages int
}

type PreparedContext struct {
	FixedReason FixedReason
	Messages    []*schema.Message
	Evidence    EvidenceSet
	Stats       ContextStats
}

type promptRule struct {
	RefID      string `json:"ref_id"`
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	ArticleID  string `json:"article_id"`
	CategoryID string `json:"category_id"`

	Version       string `json:"version"`
	Title         string `json:"title"`
	SectionPath   string `json:"section_path"`
	ArticleNumber string `json:"article_number"`

	EffectiveAt time.Time  `json:"effective_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RetrievedAt time.Time  `json:"retrieved_at"`

	ArticleContent string `json:"article_content"`
}

func BuildContext(ctx context.Context, history []llm.HistoryMessage, question string, retrieved retrieval.HybridResult, retrieveErr error) (prepared PreparedContext, err error) {
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
		}
		if err != nil {
			prepared = PreparedContext{}
		}
	}()

	if err := ctx.Err(); err != nil {
		return PreparedContext{}, err
	}
	if retrieveErr != nil {
		return PreparedContext{}, retrieveErr
	}

	query, reason, err := PrepareQuestion(ctx, question)
	if err != nil {
		return PreparedContext{}, err
	}

	stats := ContextStats{
		HistoryInputMessages: len(history),
	}
	if reason != FixedNone {
		return PreparedContext{
			FixedReason: reason,
			Stats:       stats,
		}, nil
	}

	evidence, reason, err := prepareRetrievedEvidence(ctx, retrieved)
	if err != nil {
		return PreparedContext{}, err
	}
	stats.Evidence = evidence.Stats()

	if reason != FixedNone {
		return PreparedContext{
			FixedReason: reason,
			Stats:       stats,
		}, nil
	}

	rules := make([]promptRule, 0, len(evidence.rules))
	for _, rule := range evidence.rules {
		if err := ctx.Err(); err != nil {
			return PreparedContext{}, err
		}

		citation := rule.Citation
		rules = append(rules, promptRule{
			RefID:      citation.RefID,
			DocumentID: citation.DocumentID,
			VersionID:  citation.VersionID,
			ArticleID:  citation.ArticleID,
			CategoryID: citation.CategoryID,

			Version:       citation.Version,
			Title:         citation.Title,
			SectionPath:   citation.SectionPath,
			ArticleNumber: citation.ArticleNumber,

			EffectiveAt: citation.EffectiveAt,
			ExpiresAt:   citation.ExpiresAt,
			RetrievedAt: citation.RetrievedAt,

			ArticleContent: rule.ArticleContent,
		})
	}

	rulesJSON, err := json.Marshal(rules)
	if err != nil {
		return PreparedContext{}, ErrInvalidEvidence
	}
	stats.RulesBytes = len(rulesJSON)

	if stats.RulesBytes > MaxRulesJSONBytes {
		return PreparedContext{
			FixedReason: FixedContextTooLarge,
			Stats:       stats,
		}, nil
	}

	currentJSON, err := json.Marshal(struct {
		Question string          `json:"question"`
		Rules    json.RawMessage `json:"rules"`
	}{
		Question: query,
		Rules:    json.RawMessage(rulesJSON),
	})
	if err != nil {
		return PreparedContext{}, ErrInvalidPreparation
	}
	stats.CurrentMessageBytes = len(currentJSON)

	systemText := ragSystemPrompt()
	requiredBytes := len(systemText) + len(currentJSON)

	if requiredBytes > MaxInputTextBytes {
		return PreparedContext{
			FixedReason: FixedContextTooLarge,
			Stats:       stats,
		}, nil
	}

	historyMessages, historyBytes, err := selectHistory(ctx, history, MaxInputTextBytes-requiredBytes)
	if err != nil {
		return PreparedContext{}, err
	}

	messages := make([]*schema.Message, 0, len(historyMessages)+2)
	messages = append(messages, schema.SystemMessage(systemText))
	messages = append(messages, historyMessages...)
	messages = append(messages, schema.UserMessage(string(currentJSON)))

	stats.ContextCount = len(rules)
	stats.InputTextBytes = requiredBytes + historyBytes
	stats.HistoryEvaluated = true
	stats.HistoryKeptMessages = len(historyMessages)
	stats.HistoryDroppedMessages = len(history) - len(historyMessages)

	return PreparedContext{
		Messages: messages,
		Evidence: evidence,
		Stats:    stats,
	}, nil
}

func selectHistory(ctx context.Context, history []llm.HistoryMessage, budget int) ([]*schema.Message, int, error) {
	if budget < 0 {
		return nil, 0, ErrInvalidPreparation
	}

	normalized := make([]llm.HistoryMessage, len(history))
	starts := make([]int, 0)

	for i, item := range history {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		if item.Role != "user" && item.Role != "assistant" {
			return nil, 0, ErrInvalidHistory
		}
		if !utf8.ValidString(item.Content) {
			return nil, 0, ErrInvalidHistory
		}

		content := strings.TrimSpace(item.Content)
		if content == "" {
			return nil, 0, ErrInvalidHistory
		}

		normalized[i] = llm.HistoryMessage{
			Role:    item.Role,
			Content: content,
		}
		if item.Role == "user" {
			starts = append(starts, i)
		}
	}

	selectedStart := len(normalized)
	usedBytes := 0

	for group := len(starts) - 1; group >= 0; group-- {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		end := len(normalized)
		if group+1 < len(starts) {
			end = starts[group+1]
		}

		groupBytes := 0
		fits := true

		for i := starts[group]; i < end; i++ {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}

			size := len(normalized[i].Content)
			if size > budget-usedBytes-groupBytes {
				fits = false
				break
			}
			groupBytes += size
		}

		if !fits {
			break
		}

		selectedStart = starts[group]
		usedBytes += groupBytes
	}

	messages := make([]*schema.Message, 0, len(normalized)-selectedStart)

	for _, item := range normalized[selectedStart:] {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		message := &schema.Message{
			Role:    schema.User,
			Content: item.Content,
		}
		if item.Role == "assistant" {
			message.Role = schema.Assistant
		}
		messages = append(messages, message)
	}

	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return messages, usedBytes, nil
}
