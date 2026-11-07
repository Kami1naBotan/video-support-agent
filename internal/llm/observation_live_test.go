//go:build observationlive

package llm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/observability"

	"github.com/cloudwego/eino/schema"
	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestObservationVendorRoles(t *testing.T) {
	key := os.Getenv("OBS_TEST_VENDOR_KEY")
	if key == "" {
		t.Fatal("test credential required")
	}
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	rt := &observability.Runtime{Enabled: true, Provider: provider, Registry: prometheus.NewRegistry()}
	observer, err := observability.NewLLMObserver(rt)
	if err != nil {
		t.Fatal("observer initialization failed")
	}
	models, err := NewAgentModels(context.Background(), config.LLMConfig{BaseURL: "https://api.a6api.vip/v1", Model: "gpt-6-astra", APIKey: key, Timeout: "30s"}, observer)
	if err != nil {
		t.Fatal("model initialization failed")
	}
	for _, role := range []string{"planner", "answer"} {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		messages := []*schema.Message{schema.SystemMessage("Return only OK."), schema.UserMessage("Connectivity check.")}
		var result *schema.Message
		if role == "planner" {
			result, err = models.Planner.Generate(ctx, messages)
		} else {
			result, err = models.Answer.Generate(ctx, messages)
		}
		cancel()
		if err != nil {
			t.Fatalf("%s request failed; error type=%s (provider text suppressed)", role, fmt.Sprintf("%T", err))
		}
		if result == nil {
			t.Fatal("missing model response")
		}
	}
	if len(recorder.Ended()) != 2 {
		t.Fatal("expected exactly two terminal spans")
	}
	families, err := rt.Registry.Gather()
	if err != nil {
		t.Fatal("gather failed")
	}
	if len(families) != 1 || len(families[0].Metric) != 2 {
		t.Fatal("expected distinct role metrics")
	}
	for _, m := range families[0].Metric {
		if m.GetHistogram().GetSampleCount() != 1 {
			t.Fatal("wrong role count")
		}
		labels := map[string]string{}
		for _, label := range m.Label {
			labels[label.GetName()] = label.GetValue()
		}
		// The SDK decodes one JSON object then closes the body. Depending on
		// framing/buffering, it may never request EOF. Both outcomes are part
		// of the existing transport contract; model success was asserted above.
		if labels["outcome"] != "success" && labels["outcome"] != "closed" {
			t.Fatalf("unexpected finite transport outcome: %s", labels["outcome"])
		}
		t.Logf("role=%s transport_outcome=%s count=1", labels["role"], labels["outcome"])
	}
	t.Log("real planner and answer calls each recorded once; provider content discarded; recorder only, not OTLP")
}
