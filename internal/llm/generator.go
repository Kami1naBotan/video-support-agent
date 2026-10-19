package llm

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type Generator interface {
	Generate(ctx context.Context, message []*schema.Message, opts ...einomodel.Option) (*schema.Message, error)
}
