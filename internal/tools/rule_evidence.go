package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"

	"video-support-agent/internal/model"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

type ConversationReader interface {
	GetByID(context.Context, uint64, uint64) (*model.Conversation, error)
}

var _ ConversationReader = (*service.ConversationService)(nil)

type RuleSession struct {
	caller        Caller
	conversations ConversationReader
	mu            sync.Mutex
	handle        string
	raw           []byte
}

func NewRuleSession(caller Caller, conversations ConversationReader) (*RuleSession, error) {
	if err := caller.validate(); err != nil {
		return nil, fail(err)
	}
	if conversations == nil {
		return nil, fail(ErrConfiguration)
	}
	return &RuleSession{
		caller:        caller,
		conversations: conversations,
	}, nil
}

func (s *RuleSession) authorize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.conversations == nil {
		return ErrConfiguration
	}
	if err := s.caller.validate(); err != nil {
		return err
	}

	c, err := s.conversations.GetByID(
		ctx, s.caller.userID, s.caller.conversationID,
	)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrInvalidResult
	}
	if c.ID != s.caller.conversationID || c.UserID != s.caller.userID {
		return service.ErrConversationNotFound
	}
	return ctx.Err()
}

func (s *RuleSession) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle, s.raw = "", nil
}

func (s *RuleSession) save(result retrieval.HybridResult) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", ErrInvalidResult
	}
	if len(data) > retrieval.MaxHydrationTotalBytes {
		return "", ErrResultTooLarge
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", ErrConfiguration
	}
	handle := "rules_" + hex.EncodeToString(random[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle, s.raw = handle, data
	return handle, nil
}

func (s *RuleSession) Evidence(handle string) (retrieval.HybridResult, error) {
	if s == nil {
		return retrieval.HybridResult{}, fail(ErrConfiguration)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if handle == "" || handle != s.handle || len(s.raw) == 0 {
		return retrieval.HybridResult{}, fail(service.ErrInvalidArgument)
	}

	var result retrieval.HybridResult
	if err := json.Unmarshal(s.raw, &result); err != nil {
		return retrieval.HybridResult{}, fail(ErrInvalidResult)
	}
	return result, nil
}

func ruleInfo(ctx context.Context, name, desc, definition string) (*schema.ToolInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}

	var params jsonschema.Schema
	if err := json.Unmarshal([]byte(definition), &params); err != nil {
		return nil, fail(ErrConfiguration)
	}
	return &schema.ToolInfo{
		Name:        name,
		Desc:        desc,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&params),
	}, nil
}
