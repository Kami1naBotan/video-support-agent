package rag

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"video-support-agent/internal/model"

	"github.com/cloudwego/eino/schema"
)

const MaxRawAnswerBytes = 16 << 10

var ErrInvalidAnswer = errors.New("invalid grounded model answer")

func FixedReply(ctx context.Context, reason FixedReason) (reply model.GroundedReply, err error) {
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
		}
		if err != nil {
			reply = model.GroundedReply{}
		}
	}()

	if err := ctx.Err(); err != nil {
		return model.GroundedReply{}, err
	}

	content, err := FixedText(reason)
	if err != nil {
		return model.GroundedReply{}, err
	}

	decision := model.ReplyInsufficient
	if reason == FixedQuestionTooLong || reason == FixedContextTooLarge {
		decision = model.ReplyClarify
	}

	return model.GroundedReply{
		Decision:  decision,
		Content:   content,
		Citations: make([]model.RuleCitation, 0),
	}, nil
}

func ValidateAnswer(ctx context.Context, evidence EvidenceSet, message *schema.Message) (reply model.GroundedReply, err error) {
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
		}
		if err != nil {
			reply = model.GroundedReply{}
		}
	}()

	if err := ctx.Err(); err != nil {
		return model.GroundedReply{}, err
	}

	if len(evidence.rules) == 0 || len(evidence.rules) > ChatTopK || evidence.stats.AcceptedCount != len(evidence.rules) {
		return model.GroundedReply{}, ErrInvalidEvidence
	}

	if message == nil ||
		message.Role != schema.Assistant ||
		message.ResponseMeta == nil ||
		message.ResponseMeta.FinishReason != "stop" ||
		len(message.ToolCalls) != 0 ||
		message.ToolCallID != "" ||
		message.ToolName != "" ||
		len(message.MultiContent) != 0 ||
		len(message.UserInputMultiContent) != 0 ||
		len(message.AssistantGenMultiContent) != 0 {
		return model.GroundedReply{}, ErrInvalidAnswer
	}

	if len(message.Content) == 0 || len(message.Content) > MaxRawAnswerBytes {
		return model.GroundedReply{}, ErrInvalidAnswer
	}

	raw := []byte(message.Content)
	if !validAnswerJSON(raw) {
		return model.GroundedReply{}, ErrInvalidAnswer
	}

	checked, err := decodeAnswer(ctx, raw, evidence)
	if err != nil {
		return model.GroundedReply{}, err
	}

	return renderCheckedAnswer(ctx, checked, evidence)
}

func renderCheckedAnswer(ctx context.Context, answer checkedAnswer, evidence EvidenceSet) (model.GroundedReply, error) {
	if answer.decision != model.ReplyAnswer {
		var content string

		switch answer.decision {
		case model.ReplyClarify:
			content = "请补充具体场景、适用对象或相关时间，以便判断规则是否适用。"
		case model.ReplyHandoff:
			content = "这个问题需要由官方渠道核实账号或业务信息。请通过 Bilibili 官方客服或申诉入口咨询；本项目无法代为查询或办理。"
		case model.ReplyInsufficient:
			if answer.scopeMismatch {
				content = "你已说明的适用对象与本次检索资料标注的适用范围不一致，不能直接依据这些条款判断你的权限或义务。请查阅适用于你所在地区、账号类型和具体业务的协议；如仍无法确认，请通过 Bilibili 官方客服核实。"
			} else {
				content = "本次检索资料不足以支持可靠回答。请补充规则名称或适用场景。"
			}
		default:
			return model.GroundedReply{}, ErrInvalidAnswer
		}

		return model.GroundedReply{
			Decision:  answer.decision,
			Content:   content,
			Citations: make([]model.RuleCitation, 0),
		}, nil
	}

	var content strings.Builder
	used := make(map[string]bool)

	for i, statement := range answer.statements {
		if err := ctx.Err(); err != nil {
			return model.GroundedReply{}, err
		}

		fmt.Fprintf(&content, "%d. %s", i+1, statement.text)
		for _, refID := range statement.refIDs {
			fmt.Fprintf(&content, " [%s]", refID)
			used[refID] = true
		}
		content.WriteByte('\n')
	}

	content.WriteString("\n依据（检索时快照）：\n")
	citations := make([]model.RuleCitation, 0, len(used))

	for _, rule := range evidence.rules {
		if err := ctx.Err(); err != nil {
			return model.GroundedReply{}, err
		}

		citation := rule.Citation
		if !used[citation.RefID] {
			continue
		}

		number := citation.ArticleNumber
		if strings.TrimSpace(number) == "" {
			number = "原文未编号"
		}

		fmt.Fprintf(&content, "[%s] 规则：%s；条款：%s；版本：%s\n来源：%s\n", citation.RefID,
			strconv.Quote(citation.Title),
			strconv.Quote(number),
			strconv.Quote(citation.Version),
			strconv.Quote(citation.SourceURL),
		)

		citations = append(citations, citation.Clone())
	}

	return model.GroundedReply{
		Decision:  model.ReplyAnswer,
		Content:   strings.TrimSpace(content.String()),
		Citations: citations,
	}, nil
}
