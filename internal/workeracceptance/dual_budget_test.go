//go:build workerdual

package workeracceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgefailure"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/model"
)

type wrongDigestEmbedding struct{ controlledEmbedding }

func (e wrongDigestEmbedding) InspectIdentity(ctx context.Context) (embedding.ModelIdentity, error) {
	id, err := e.controlledEmbedding.InspectIdentity(ctx)
	id.Digest = strings.Repeat("0", 64)
	return id, err
}

// Each boundary seeds only the preceding failure count, then performs a real build
// and recovery transaction. This is not a claim of five wall-clock retries.
func TestWorkerDualFailureBudget(t *testing.T) {
	for attempt := uint32(1); attempt <= 5; attempt++ {
		t.Run(fmt.Sprintf("transient_attempt_%d", attempt), func(t *testing.T) { testDualFailureBoundary(t, attempt, false) })
	}
	t.Run("permanent_identity", func(t *testing.T) { testDualFailureBoundary(t, 1, true) })
}
func testDualFailureBoundary(t *testing.T, attempt uint32, permanent bool) {
	f := newDual(t)
	f.source(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	must(t, f.db.Model(&model.KnowledgeIndexTask{}).Where("source_epoch = 1").Update("failed_attempts", attempt-1).Error)
	claim, err := f.repo.ClaimLatest(ctx)
	must(t, err)
	if claim == nil || claim.Attempt != attempt {
		t.Fatal("incorrect seeded attempt")
	}
	var embedder knowledgeworker.IdentityEmbedder = controlledEmbedding{}
	code, expected := knowledgefailure.CodeDualExternalFailed, knowledgeworker.ErrDualExternal
	if permanent {
		embedder = wrongDigestEmbedding{}
		code, expected = knowledgefailure.CodeDualInputInvalid, knowledgeworker.ErrDualPermanent
	}
	builder, err := knowledgeworker.NewDualBuilder(f.repo, embedder, lostESVerification{f.es}, f.vector, f.options)
	must(t, err)
	verified, err := builder.Build(ctx, *claim)
	if verified != nil || !errors.Is(err, expected) {
		t.Fatalf("unexpected build result: %v", err)
	}
	must(t, f.repo.RecordBuildFailure(ctx, *claim, code))
	var original model.KnowledgeBuildRecovery
	must(t, f.db.Take(&original).Error)
	must(t, f.repo.RecordBuildFailure(ctx, *claim, code))
	_, err = f.repo.RecoverPending(ctx)
	must(t, err)
	_, err = f.repo.RecoverPending(ctx)
	must(t, err)
	var recovery model.KnowledgeBuildRecovery
	var task model.KnowledgeIndexTask
	var control model.KnowledgeIndexControl
	must(t, f.db.Take(&recovery).Error)
	must(t, f.db.Take(&task).Error)
	must(t, f.db.Take(&control).Error)
	if task.FailedAttempts != attempt || control.ActiveGenerationID != nil || recovery.Status != model.KnowledgeRecoveryCompleted || recovery.JitterPermille != original.JitterPermille || !recovery.DecidedAt.Equal(original.DecidedAt) {
		t.Fatal("failure replay changed budget, decision or active")
	}
	if permanent || attempt == 5 {
		if task.Status != model.KnowledgeTaskBlocked || recovery.Action != string(knowledgefailure.ActionDeadLetter) || recovery.NotBefore != nil {
			t.Fatal("terminal failure scheduled retry")
		}
		next, err := f.repo.ClaimLatest(ctx)
		if next != nil || !errors.Is(err, knowledgeworker.ErrBuildExhausted) {
			t.Fatalf("terminal claim=%v", err)
		}
		var retries int64
		must(t, f.db.Model(&model.EventOutbox{}).Where("topic = ?", eventbus.TopicIndexRetry).Count(&retries).Error)
		if retries != 0 {
			t.Fatal("terminal retry emitted")
		}
		var event model.KnowledgeEventInbox
		must(t, f.db.Take(&event).Error)
		if event.Status != model.KnowledgeEventDeadLettered {
			t.Fatal("terminal event not dead-lettered")
		}
		workerSQLCount(t, f.db, &model.EventOutbox{}, 2)
	} else {
		bases := []time.Duration{5 * time.Second, 30 * time.Second, 120 * time.Second, 300 * time.Second}
		base := bases[attempt-1]
		expectedAt := recovery.DecidedAt.UTC().Truncate(time.Millisecond).Add(base + base*time.Duration(recovery.JitterPermille)/1000)
		if recovery.NotBefore == nil || original.NotBefore == nil || !recovery.NotBefore.Equal(*original.NotBefore) || !recovery.NotBefore.Equal(expectedAt) || recovery.JitterPermille > 200 || !task.NotBefore.Equal(expectedAt) || task.Status != model.KnowledgeTaskRetryWait {
			t.Fatal("durable backoff/replay mismatch")
		}
		workerSQLCount(t, f.db, &model.EventOutbox{}, 1)
	}
	workerSQLCount(t, f.db, &model.KnowledgeIndexGeneration{}, 1)
	t.Logf("attempt=%d permanent=%t failed=%d action=%s jitter=%d not_before=%v active=nil", attempt, permanent, task.FailedAttempts, recovery.Action, recovery.JitterPermille, recovery.NotBefore)
}
