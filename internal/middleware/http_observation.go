package middleware

import (
	"errors"
	"net/http"
	"time"

	"video-support-agent/internal/observability"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const httpPanicKey = "observability.http.panic"

func NewHTTPObservation(runtime *observability.Runtime) (gin.HandlerFunc, error) {
	if runtime == nil {
		return nil, errors.New("HTTP observation runtime is required")
	}
	if !runtime.Enabled {
		return func(c *gin.Context) {
			c.Next()
		}, nil
	}
	if runtime.Registry == nil || runtime.Provider == nil || runtime.Propagator == nil {
		return nil, errors.New("HTTP observation dependencies are required")
	}

	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "http_server_request_duration_seconds",
			Help: "HTTP handler duration in seconds, including streaming.",
			Buckets: []float64{
				0.005, 0.01, 0.025, 0.05, 0.1,
				0.25, 0.5, 1, 2.5, 5,
				10, 20, 40, 60,
			},
		},
		[]string{"method", "route", "status_class"},
	)
	if err := runtime.Registry.Register(duration); err != nil {
		return nil, errors.New("register HTTP duration metric failed")
	}

	tracer := runtime.Provider.Tracer("video-support-agent/http")
	return httpObservationHandler(runtime.Propagator, tracer, duration), nil
}

func httpObservationHandler(propagator propagation.TextMapPropagator, tracer trace.Tracer, duration *prometheus.HistogramVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		method := normalizedHTTPMethod(c.Request.Method)

		parent := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		ctx, span := tracer.Start(parent, method, trace.WithSpanKind(trace.SpanKindServer))
		c.Request = c.Request.WithContext(ctx)

		defer func() {
			defer span.End()

			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}

			status := c.Writer.Status()
			span.SetName(method + " " + route)
			span.SetAttributes(
				attribute.String("http.request.method", method),
				attribute.String("http.route", route),
				attribute.Int("http.response.status_code", status),
			)

			switch {
			case c.GetBool(httpPanicKey):
				span.SetAttributes(attribute.Bool("app.http.panic", true))
				span.SetStatus(codes.Error, "handler_panic")
			case status >= http.StatusInternalServerError:
				span.SetStatus(codes.Error, "server_error")
			}

			duration.WithLabelValues(method, route, httpStatusClass(status)).Observe(time.Since(started).Seconds())
		}()

		c.Next()
	}
}

func normalizedHTTPMethod(method string) string {
	switch method {
	case http.MethodGet,
		http.MethodHead,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodConnect,
		http.MethodOptions,
		http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func httpStatusClass(status int) string {
	switch {
	case status >= 100 && status < 200:
		return "1xx"
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}
