package tools

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace/noop"
	"testing"
	"video-support-agent/internal/observability"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type observedBoundaryTool struct{ calls int }

func (x *observedBoundaryTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: SearchRuleName}, nil
}
func (x *observedBoundaryTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	x.calls++
	return "", errors.New("synthetic execution failure")
}

func TestRegistryObservesOnlyActualEntry(t *testing.T) {
	for _, mode := range []string{"entered_failure", "unknown_name", "lock_rejected"} {
		t.Run(mode, func(t *testing.T) {
			target := &observedBoundaryTool{}
			metrics := prometheus.NewRegistry()
			observation, observerErr := observability.NewExecutionObserver(&observability.Runtime{Enabled: true, Registry: metrics, Provider: noop.NewTracerProvider()})
			if observerErr != nil {
				t.Fatal(observerErr)
			}
			registry := &Registry{entries: map[string]tool.InvokableTool{SearchRuleName: target}, create: &CreateAppealTicketTool{}, observation: observation}
			ctx, trace, e := NewEvaluationTrace(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			ctx = WithEvaluationCall(ctx, EvaluationCallIdentity{RunID: "11111111-1111-4111-8111-111111111111", CallID: "22222222-2222-4222-8222-222222222222", Sequence: 1})
			name := SearchRuleName
			if mode == "unknown_name" {
				name = "unknown"
			}
			if mode == "lock_rejected" {
				registry.mu.Lock()
				defer registry.mu.Unlock()
			}
			if _, e = registry.invoke(ctx, name, `{"query":"synthetic"}`); e == nil {
				t.Fatal("fixture expected failure")
			}
			rows, e := trace.Take()
			if e != nil {
				t.Fatal(e)
			}
			want := 0
			if mode == "entered_failure" {
				want = 1
			}
			families, gatherErr := metrics.Gather()
			if gatherErr != nil {
				t.Fatal(gatherErr)
			}
			var counted float64
			for _, family := range families {
				if family.GetName() == "tool_calls_total" {
					for _, metric := range family.Metric {
						counted += metric.GetCounter().GetValue()
					}
				}
			}
			if counted != float64(want) {
				t.Fatalf("tool metric=%v want=%d", counted, want)
			}
			if len(rows) != want || target.calls != want {
				t.Fatalf("calls=%d records=%d want=%d", target.calls, len(rows), want)
			}
		})
	}
}
