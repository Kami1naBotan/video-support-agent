package agent

import (
	"context"
	"testing"
	"video-support-agent/internal/observability"
	"video-support-agent/internal/service"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestObservationAgentConfirmedDispatch(t *testing.T) {
	r := newLoopRig(t)
	executor := &Executor{ConfirmedExecutor: r.confirmRig.executor, models: r.exec.models}
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	rt := &observability.Runtime{Enabled: true, Registry: prometheus.NewRegistry(), Provider: provider}
	observation, err := observability.NewExecutionObserver(rt)
	require.NoError(t, err)
	wrapped, err := ObserveExecutor(executor, observation)
	require.NoError(t, err)
	out, err := wrapped.Execute(context.Background(), service.AgentRequest{UserID: 7, ConversationID: 9, Confirmed: &service.AgentConfirmation{Input: r.request.Input, Confirmation: r.request.Confirmation}}, nil)
	require.NoError(t, err)
	require.Equal(t, "committed", out.BusinessEffect)
	require.Equal(t, 1, r.txCalls)
	require.Len(t, recorder.Ended(), 1)
}
func TestObservationAgentTerminalClassification(t *testing.T) {
	for _, tc := range []struct {
		effect  string
		pending bool
		err     error
		want    string
	}{
		{"committed", true, nil, "failed"}, {"unknown", false, context.DeadlineExceeded, "unknown"}, {"none", false, context.Canceled, "cancelled"}, {"none", false, nil, "completed"},
	} {
		out := &service.AgentOutcome{ExecutionStatus: "completed", BusinessEffect: tc.effect, AuditPending: tc.pending, Decision: "clarify"}
		got := observedAgentResult(out, tc.err, true)
		require.Equal(t, tc.want, got.Outcome)
		require.Equal(t, tc.effect, got.BusinessEffect)
		require.Equal(t, "completed", out.ExecutionStatus)
	}
}
