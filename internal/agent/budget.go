package agent

import (
	"context"
	"time"

	"video-support-agent/internal/service"
)

type runBudget struct {
	total context.Context
	work  context.Context
	close func()
}

func newRunBudget(parent context.Context) *runBudget {
	started := time.Now()

	total, cancelTotal := context.WithDeadline(parent, started.Add(40*time.Second))

	shared, release := service.WithAppealFinalization(total)

	deadline, _ := total.Deadline()
	workDeadline := started.Add(36 * time.Second)

	if reserved := deadline.Add(-4 * time.Second); reserved.Before(workDeadline) {
		workDeadline = reserved
	}

	work, cancelWork := context.WithDeadline(shared, workDeadline)

	return &runBudget{
		total: shared,
		work:  work,
		close: func() {
			cancelWork()
			release()
			cancelTotal()
		},
	}
}
