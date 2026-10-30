package elasticsearch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"video-support-agent/internal/config"
)

const (
	MaxRequestBytes  = 4 << 20
	MaxResponseBytes = 2 << 20
)

var (
	ErrUnavailable  = errors.New("elasticsearch unavailable")
	ErrResponse     = errors.New("invalid elasticsearch response")
	ErrRejected     = errors.New("elasticsearch operation rejected")
	ErrStateUnknown = errors.New("elasticsearch operation state unknown")
)

type Client struct {
	baseURL    string
	alias      string
	httpClient *http.Client
	transport  *http.Transport
}

func NewClient(cfg config.ElasticsearchConfig) (*Client, error) {
	if err := config.ValidateElasticsearchConfig(cfg); err != nil {
		return nil, err
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return nil, errors.New("invalid ES_TIMEOUT")
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   5,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(
			req *http.Request,
			via []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}

	return &Client{
		baseURL:    strings.TrimRight(cfg.URL, "/"),
		alias:      cfg.IndexAlias,
		httpClient: httpClient,
		transport:  transport,
	}, nil
}

func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}

func (c *Client) request(ctx context.Context, method string, path string, contentType string, body []byte) (int, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if len(body) > MaxRequestBytes {
		return 0, nil, fmt.Errorf("%w: request body exceeds %d bytes", ErrRejected, MaxRequestBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: cannot build request", ErrRejected)
	}

	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	response, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, requestError(ctx, err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, int64(MaxResponseBytes)+1))
	if err != nil {
		return response.StatusCode, nil, requestError(ctx, err)
	}
	if len(data) > MaxResponseBytes {
		return response.StatusCode, nil, fmt.Errorf("%w: response body exceeds %d bytes", ErrResponse, MaxResponseBytes)
	}

	if err := ctx.Err(); err != nil {
		return response.StatusCode, nil, err
	}

	return response.StatusCode, data, nil
}

func requestError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrUnavailable, context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}

	return fmt.Errorf("%w: request or response read failed", ErrUnavailable)
}
