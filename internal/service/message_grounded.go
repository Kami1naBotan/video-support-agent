package service

import (
	"context"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/rag"
)

type GroundedChatResponder interface {
	ReplyGrounded(context.Context, []llm.HistoryMessage, string) (model.GroundedReply, error)
}

var (
	_ ChatResponder         = (*rag.Responder)(nil)
	_ GroundedChatResponder = (*rag.Responder)(nil)
)

type messageReply struct {
	content   string
	citations model.MessageCitations
}

func (s *MessageService) replyForMessage(ctx context.Context, history []llm.HistoryMessage, question string) (messageReply, error) {
	if err := ctx.Err(); err != nil {
		return messageReply{}, err
	}

	if s.groundedChat != nil {
		return s.replyGroundedForMessage(ctx, history, question)
	}

	if s.chat == nil {
		return messageReply{}, llm.ErrModelFailed
	}

	answer, err := s.chat.Reply(ctx, history, question)
	if err != nil {
		return messageReply{}, err
	}

	answer = strings.TrimSpace(answer)
	if answer == "" {
		return messageReply{}, llm.ErrEmptyReply
	}

	return messageReply{
		content:   answer,
		citations: model.MessageCitations{},
	}, nil
}

func (s *MessageService) replyGroundedForMessage(ctx context.Context, history []llm.HistoryMessage, question string) (messageReply, error) {
	if err := ctx.Err(); err != nil {
		return messageReply{}, err
	}
	if s.groundedChat == nil {
		return messageReply{}, llm.ErrModelFailed
	}

	reply, err := s.groundedChat.ReplyGrounded(ctx, history, question)

	if contextErr := ctx.Err(); contextErr != nil {
		return messageReply{}, contextErr
	}
	if err != nil {
		return messageReply{}, err
	}

	if !utf8.ValidString(reply.Content) || strings.TrimSpace(reply.Content) == "" {
		return messageReply{}, rag.ErrInvalidAnswer
	}

	switch reply.Decision {
	case model.ReplyAnswer:
		if len(reply.Citations) == 0 || len(reply.Citations) > rag.ChatTopK {
			return messageReply{}, rag.ErrInvalidAnswer
		}

	case model.ReplyClarify, model.ReplyHandoff, model.ReplyInsufficient:
		if len(reply.Citations) != 0 {
			return messageReply{}, rag.ErrInvalidAnswer
		}

	default:
		return messageReply{}, rag.ErrInvalidAnswer
	}

	citations := model.NewMessageCitations(reply.Citations)

	if _, err := citations.Value(); err != nil {
		return messageReply{}, rag.ErrInvalidAnswer
	}

	if err := ctx.Err(); err != nil {
		return messageReply{}, err
	}

	return messageReply{
		content:   reply.Content,
		citations: citations,
	}, nil
}

func (s *MessageService) streamReplyForMessage(ctx context.Context, history []llm.HistoryMessage, question string, onChunk func(string) error) (messageReply, error) {
	if s.groundedChat != nil {
		reply, err := s.replyGroundedForMessage(ctx, history, question)
		if err != nil {
			return messageReply{}, err
		}

		if err := ctx.Err(); err != nil {
			return messageReply{}, err
		}

		if err := onChunk(reply.content); err != nil {
			return messageReply{}, err
		}

		return reply, nil
	}

	if s.streamChat == nil {
		return messageReply{}, llm.ErrStreamUnsupported
	}

	answer, err := s.streamChat.ReplyStream(
		ctx,
		history,
		question,
		func(part string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return onChunk(part)
		},
	)
	if err != nil {
		return messageReply{}, err
	}

	if strings.TrimSpace(answer) == "" {
		return messageReply{}, llm.ErrEmptyReply
	}

	return messageReply{
		content:   answer,
		citations: model.MessageCitations{},
	}, nil
}
