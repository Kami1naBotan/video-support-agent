package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

const QueryTicketStatusName = "query_ticket_status"

type TicketStatusReader interface {
	Query(context.Context, uint64, uint64, uint64) (*service.AppealTicketView, error)
}

var _ TicketStatusReader = (*service.AppealTicketService)(nil)
var _ tool.InvokableTool = (*QueryTicketStatusTool)(nil)

type QueryTicketStatusTool struct {
	caller  Caller
	tickets TicketStatusReader
}

func NewQueryTicketStatusTool(caller Caller, tickets TicketStatusReader) (*QueryTicketStatusTool, error) {
	if err := caller.validate(); err != nil {
		return nil, fail(err)
	}
	if tickets == nil {
		return nil, fail(ErrConfiguration)
	}
	return &QueryTicketStatusTool{
		caller: caller, tickets: tickets,
	}, nil
}

type queryTicketArgs struct {
	TicketID string `json:"ticket_id"`
}

type TicketStatusResult struct {
	TicketID  string                   `json:"ticket_id"`
	Status    model.AppealTicketStatus `json:"status"`
	CreatedAt time.Time                `json:"created_at"`
	UpdatedAt time.Time                `json:"updated_at"`
}

func (t *QueryTicketStatusTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}

	const definition = `{
		"type":"object",
		"properties":{
			"ticket_id":{
				"type":"string",
				"description":"本项目工单ID，规范正十进制字符串。",
				"pattern":"^[1-9][0-9]{0,19}$",
				"minLength":1,
				"maxLength":20
			}
		},
		"required":["ticket_id"],
		"additionalProperties":false
	}`

	var params jsonschema.Schema
	if err := json.Unmarshal([]byte(definition), &params); err != nil {
		return nil, fail(ErrConfiguration)
	}

	return &schema.ToolInfo{
		Name:        QueryTicketStatusName,
		Desc:        "查询当前用户自己的本项目申诉工单状态。" + "用户身份与会话由服务端提供，不接受身份参数。" + "不代表外部平台工单状态。",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&params),
	}, nil
}

func (t *QueryTicketStatusTool) Execute(ctx context.Context, raw string) (*TicketStatusResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}
	if t == nil || t.tickets == nil {
		return nil, fail(ErrConfiguration)
	}
	if err := t.caller.validate(); err != nil {
		return nil, fail(err)
	}

	var args queryTicketArgs
	if err := decodeObject(raw, &args, "ticket_id"); err != nil {
		return nil, fail(err)
	}
	id, err := parseID(args.TicketID)
	if err != nil {
		return nil, fail(err)
	}

	work, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	view, err := t.tickets.Query(
		work, t.caller.userID, t.caller.conversationID, id,
	)
	if err != nil {
		return nil, fail(err)
	}
	if err := work.Err(); err != nil {
		return nil, fail(err)
	}

	if view == nil || view.TicketID != id || view.CreatedAt.IsZero() || view.UpdatedAt.Before(view.CreatedAt) {
		return nil, fail(ErrInvalidResult)
	}
	switch view.Status {
	case model.AppealTicketSubmitted, model.AppealTicketPendingReview:
	default:
		return nil, fail(ErrInvalidResult)
	}

	return &TicketStatusResult{
		TicketID:  strconv.FormatUint(view.TicketID, 10),
		Status:    view.Status,
		CreatedAt: view.CreatedAt.UTC(),
		UpdatedAt: view.UpdatedAt.UTC(),
	}, nil
}

func (t *QueryTicketStatusTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	result, err := t.Execute(ctx, raw)
	if err != nil {
		return "", err
	}
	return encodeResult(result)
}
