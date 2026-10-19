package handler

import (
	"context"

	"video-support-agent/internal/llm"
)

type testChatResponder struct{}

func (testChatResponder) Reply(
	_ context.Context,
	_ []llm.HistoryMessage,
	_ string,
) (string, error) {
	return "测试模型回复", nil
}
