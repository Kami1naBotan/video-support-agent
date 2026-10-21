package llm

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

func buildMessage(history []HistoryMessage, question string) ([]*schema.Message, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, ErrInvalidInput
	}

	messages := make([]*schema.Message, 0, len(history)+2)
	messages = append(messages, schema.SystemMessage(supportSystemPrompt))

	for _, item := range history {
		content := strings.TrimSpace(item.Content)
		if content == "" {
			return nil, ErrInvalidInput
		}

		switch item.Role {
		case "user":
			messages = append(messages, schema.UserMessage(content))
		case "assistant":
			messages = append(messages, &schema.Message{
				Role:    schema.Assistant,
				Content: content,
			})
		default:
			return nil, ErrInvalidInput
		}
	}

	messages = append(messages, schema.UserMessage(question))

	last := len(messages) - 1
	requiredBytes := len(messages[0].Content) + len(messages[last].Content)
	if requiredBytes > maxInputTextBytes {
		return nil, ErrInputTooLarge
	}

	totalBytes := requiredBytes
	for _, message := range messages[1:last] {
		totalBytes += len(message.Content)
	}

	start := 1
	for totalBytes > maxInputTextBytes && start < last {
		totalBytes -= len(messages[start].Content)
		start++

		for start < last && messages[start].Role != schema.User {
			totalBytes -= len(messages[start].Content)
			start++
		}
	}

	if start > 1 {
		trimmed := make([]*schema.Message, 0, len(messages)-start+1)
		trimmed = append(trimmed, messages[0])
		trimmed = append(trimmed, messages[start:last]...)
		trimmed = append(trimmed, messages[last])
		messages = trimmed
	}

	return messages, nil
}
