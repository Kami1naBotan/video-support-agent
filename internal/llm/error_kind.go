package llm

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/cloudwego/eino-ext/components/model/openai"
)

type ErrorKind string

const (
	ErrorKindAuthentication ErrorKind = "authentication"
	ErrorKindPermission     ErrorKind = "permission"
	ErrorKindRateOrQuota    ErrorKind = "rate_or_quota"
	ErrorKindUpstream       ErrorKind = "upstream"
	ErrorKindRequest        ErrorKind = "request_rejected"
	ErrorKindTimeout        ErrorKind = "timeout"
	ErrorKindCanceled       ErrorKind = "canceled"
	ErrorKindNetwork        ErrorKind = "network"
	ErrorKindUnknown        ErrorKind = "unknown"
)

func classifyModelError(err error) ErrorKind {
	if errors.Is(err, context.Canceled) {
		return ErrorKindCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorKindTimeout
	}

	var apiErr *openai.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		switch {
		case apiErr.HTTPStatusCode == http.StatusUnauthorized:
			return ErrorKindAuthentication
		case apiErr.HTTPStatusCode == http.StatusForbidden:
			return ErrorKindPermission
		case apiErr.HTTPStatusCode == http.StatusTooManyRequests:
			return ErrorKindRateOrQuota
		case apiErr.HTTPStatusCode == http.StatusRequestTimeout || apiErr.HTTPStatusCode == http.StatusGatewayTimeout:
			return ErrorKindTimeout
		case apiErr.HTTPStatusCode >= 500 && apiErr.HTTPStatusCode <= 599:
			return ErrorKindUpstream
		case apiErr.HTTPStatusCode >= 400 && apiErr.HTTPStatusCode <= 499:
			return ErrorKindRequest
		default:
			return ErrorKindUnknown
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ErrorKindTimeout
		}
		return ErrorKindNetwork
	}

	return ErrorKindUnknown
}
