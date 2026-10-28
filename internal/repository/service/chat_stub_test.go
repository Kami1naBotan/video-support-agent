package service

import (
	"context"

	"video-support-agent/internal/llm"
)

const demoReply = "测试模型回复"

type fakeChatResponder struct{}

func (fakeChatResponder) Reply(
	_ context.Context,
	_ []llm.HistoryMessage,
	_ string,
) (string, error) {
	return demoReply, nil
}
