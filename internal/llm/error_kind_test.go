package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/openai"
)

func TestClassifyModelErrorHTTP(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   ErrorKind
	}{
		{"authentication", http.StatusUnauthorized, ErrorKindAuthentication},
		{"permission", http.StatusForbidden, ErrorKindPermission},
		{"rate or quota", http.StatusTooManyRequests, ErrorKindRateOrQuota},
		{"request timeout", http.StatusRequestTimeout, ErrorKindTimeout},
		{"gateway timeout", http.StatusGatewayTimeout, ErrorKindTimeout},
		{"server failure", http.StatusInternalServerError, ErrorKindUpstream},
		{"service unavailable", http.StatusServiceUnavailable, ErrorKindUpstream},
		{"bad request", http.StatusBadRequest, ErrorKindRequest},
		{"model or route not found", http.StatusNotFound, ErrorKindRequest},
		{"missing status", 0, ErrorKindUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamErr := &openai.APIError{
				HTTPStatusCode: tt.status,
				Message:        "sensitive-marker: simulated private response",
				Code:           "untrusted-provider-code",
				Type:           "untrusted-provider-type",
			}

			// 模拟错误被中间层包装。
			err := fmt.Errorf("model adapter: %w", upstreamErr)

			got := classifyModelError(err)
			if got != tt.want {
				t.Fatalf("expected category %q, got %q", tt.want, got)
			}
		})
	}
}

func TestClassifyModelErrorOther(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorKind
	}{
		{
			name: "canceled",
			err:  fmt.Errorf("request: %w", context.Canceled),
			want: ErrorKindCanceled,
		},
		{
			name: "deadline exceeded",
			err:  fmt.Errorf("request: %w", context.DeadlineExceeded),
			want: ErrorKindTimeout,
		},
		{
			name: "network timeout",
			err: &net.DNSError{
				Err:       "simulated DNS timeout",
				Name:      "example.invalid",
				IsTimeout: true,
			},
			want: ErrorKindTimeout,
		},
		{
			name: "network failure",
			err: &net.DNSError{
				Err:        "simulated DNS failure",
				Name:       "example.invalid",
				IsNotFound: true,
			},
			want: ErrorKindNetwork,
		},
		{
			name: "unknown error",
			err:  errors.New("sensitive-marker: unexpected upstream detail"),
			want: ErrorKindUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyModelError(tt.err)
			if got != tt.want {
				t.Fatalf("expected category %q, got %q", tt.want, got)
			}
		})
	}
}
