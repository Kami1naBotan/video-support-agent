package tools

import (
	"context"
	"reflect"
	"sync"

	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type AppealToolsService interface {
	EligibilityReader
	AppealCreator
	TicketStatusReader
}

var _ AppealToolsService = (*service.AppealTicketService)(nil)

type RegistryDependencies struct {
	Conversations ConversationReader
	Retriever     retrieval.HybridRetriever
	Knowledge     RuleDetailReader
	Appeals       AppealToolsService
	WriteAudit    CreateWriteAudit
}

type Registry struct {
	mu      sync.Mutex
	entries map[string]tool.InvokableTool
	order   []string
	rules   *RuleSession
	create  *CreateAppealTicketTool
}

func NewRegistry(caller Caller, deps RegistryDependencies, confirmed *ConfirmedAppeal) (*Registry, error) {
	for _, dependency := range []any{
		deps.Conversations,
		deps.Retriever,
		deps.Knowledge,
		deps.Appeals,
	} {
		if nilDependency(dependency) {
			return nil, fail(ErrConfiguration)
		}
	}
	if nilDependency(deps.WriteAudit) {
		deps.WriteAudit = nil
	}

	session, err := NewRuleSession(caller, deps.Conversations)
	if err != nil {
		return nil, err
	}
	search, err := NewSearchRuleTool(session, deps.Retriever)
	if err != nil {
		return nil, err
	}
	detail, err := NewGetRuleDetailTool(session, deps.Knowledge)
	if err != nil {
		return nil, err
	}
	eligibility, err := NewCheckAppealEligibilityTool(
		caller, deps.Appeals,
	)
	if err != nil {
		return nil, err
	}
	create, err := NewCreateAppealTicketTool(
		session, deps.Appeals, deps.WriteAudit, confirmed,
	)
	if err != nil {
		return nil, err
	}
	query, err := NewQueryTicketStatusTool(caller, deps.Appeals)
	if err != nil {
		return nil, err
	}

	return &Registry{
		entries: map[string]tool.InvokableTool{
			SearchRuleName:             search,
			GetRuleDetailName:          detail,
			CheckAppealEligibilityName: eligibility,
			CreateAppealTicketName:     create,
			QueryTicketStatusName:      query,
		},
		order: []string{
			SearchRuleName,
			GetRuleDetailName,
			CheckAppealEligibilityName,
			CreateAppealTicketName,
			QueryTicketStatusName,
		},
		rules:  session,
		create: create,
	}, nil
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (r *Registry) Infos(ctx context.Context) ([]*schema.ToolInfo, error) {
	if r == nil {
		return nil, fail(ErrConfiguration)
	}

	infos := make([]*schema.ToolInfo, 0, len(r.order))
	for _, name := range r.order {
		info, err := r.entries[name].Info(ctx)
		if err != nil {
			return nil, err
		}
		if info == nil || info.Name != name {
			return nil, fail(ErrConfiguration)
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func (r *Registry) Invoke(ctx context.Context, name, raw string) (string, error) {
	if r == nil || r.create == nil {
		return "", fail(ErrConfiguration)
	}
	if r.create.confirmed != nil {
		return "", fail(service.ErrAppealConfirmationRequired)
	}
	return r.invoke(ctx, name, raw)
}

func (r *Registry) InvokeConfirmed(ctx context.Context) (string, error) {
	if r == nil || r.create == nil {
		return "", fail(ErrConfiguration)
	}
	if r.create.confirmed == nil {
		return "", fail(service.ErrAppealConfirmationRequired)
	}

	raw, err := encodeResult(r.create.confirmed.Input)
	if err != nil {
		return "", err
	}
	return r.invoke(ctx, CreateAppealTicketName, raw)
}

func (r *Registry) invoke(ctx context.Context, name, raw string) (string, error) {
	if !r.mu.TryLock() {
		return "", fail(service.ErrInvalidArgument)
	}
	defer r.mu.Unlock()

	entry, exists := r.entries[name]
	if !exists {
		return "", fail(service.ErrInvalidArgument)
	}
	return entry.InvokableRun(ctx, raw)
}

func (r *Registry) RuleEvidence(handle string) (retrieval.HybridResult, error) {
	return r.rules.Evidence(handle)
}

func (r *Registry) Proposal() *service.PreparedAppeal {
	return r.create.Proposal()
}

func (r *Registry) BusinessResult() (*service.AppealCreateResult, string) {
	return r.create.BusinessResult()
}
