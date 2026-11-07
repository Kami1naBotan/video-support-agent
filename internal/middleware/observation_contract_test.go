package middleware

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"net/http/httptest"
	"testing"
	"video-support-agent/internal/observability"
)

func TestObservationHTTPPrivacyAndPanic(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	rt := &observability.Runtime{Enabled: true, Registry: prometheus.NewRegistry(), Provider: provider, Propagator: propagation.TraceContext{}}
	observation, err := NewHTTPObservation(rt)
	require.NoError(t, err)
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	router := gin.New()
	router.Use(observation, SafeAccessLog(logger), SafeRecovery(logger))
	router.GET("/items/:id", func(c *gin.Context) {
		c.String(200, "data: test\n\n")
		c.Writer.Flush()
		panic("synthetic-private-panic")
	})
	request := httptest.NewRequest("GET", "/items/synthetic-private-user?key=synthetic-private-query", nil)
	request.Header.Set("Authorization", "Bearer synthetic-private-key")
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	router.ServeHTTP(httptest.NewRecorder(), request)
	require.Len(t, recorder.Ended(), 1)
	span := recorder.Ended()[0]
	require.Equal(t, "2222222222222222", span.Parent().SpanID().String())
	require.Equal(t, codes.Error, span.Status().Code)
	require.Equal(t, "GET /items/:id", span.Name())
	require.NotContains(t, fmt.Sprint(span.Attributes()), "synthetic-private")
	for _, entry := range logs.All() {
		require.NotContains(t, fmt.Sprint(entry.ContextMap()), "synthetic-private")
	}
	families, err := rt.Registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		require.NotContains(t, family.String(), "synthetic-private")
	}
}
