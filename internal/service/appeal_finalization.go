package service

import (
	"context"
	"sync"
	"time"
)

type appealFinalizationKey struct{}

type appealFinalizationBudget struct {
	parent context.Context
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
}

func (b *appealFinalizationBudget) start() {
	b.once.Do(func() {
		deadline := time.Now().Add(3 * time.Second)
		if earlier, ok := b.parent.Deadline(); ok && earlier.Before(deadline) {
			deadline = earlier
		}
		b.ctx, b.cancel = context.WithDeadline(context.WithoutCancel(b.parent), deadline)
	})
}

func WithAppealFinalization(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Value(appealFinalizationKey{}).(*appealFinalizationBudget); ok {
		return ctx, func() {}
	}
	budget := &appealFinalizationBudget{parent: ctx}
	return context.WithValue(ctx, appealFinalizationKey{}, budget), func() {
		budget.start()
		budget.cancel()
	}
}

func AppealFinalizationContext(ctx context.Context) (context.Context, error) {
	budget, ok := ctx.Value(appealFinalizationKey{}).(*appealFinalizationBudget)
	if !ok {
		return nil, ErrAppealConfiguration
	}
	budget.start()
	return context.WithValue(budget.ctx, appealFinalizationKey{}, budget), nil
}
