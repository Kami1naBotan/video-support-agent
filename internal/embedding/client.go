package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/config"
)

const (
	maxRequestBytes  = 2 << 20
	maxResponseBytes = 4 << 20
)

type Client struct {
	baseURL   string
	model     string
	digest    string
	timeout   time.Duration
	http      *http.Client
	transport *http.Transport
}

var _ Embedder = (*Client)(nil)

func NewClient(cfg config.EmbeddingConfig) (*Client, error) {
	if err := config.ValidateEmbeddingConfig(cfg); err != nil {
		return nil, err
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return nil, errors.New("invalid EMBEDDING_TIMEOUT")
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}

	return &Client{
		baseURL: strings.TrimRight(cfg.URL, "/"),
		model:   cfg.Model,
		digest:  cfg.ModelDigest,
		timeout: timeout,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		transport: transport,
	}, nil
}

func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}

func (c *Client) request(ctx context.Context, method string, path string, payload any) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot encode request", ErrInput)
		}
	}
	if len(body) > maxRequestBytes {
		return nil, fmt.Errorf("%w: encoded request exceeds byte budget", ErrInputTooLarge)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", ErrInput)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, int64(maxResponseBytes)+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds byte budget", ErrResponse)
	}

	if response.StatusCode != http.StatusOK {
		if path == "/api/embed" && response.StatusCode == http.StatusBadRequest {
			var failure struct {
				Error string `json:"error"`
			}
			if utf8.Valid(data) && json.Unmarshal(data, &failure) == nil && failure.Error == "the input length exceeds the context length" {
				return nil, ErrContextLength
			}
		}

		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, response.StatusCode)
		}

		return nil, fmt.Errorf("%w: HTTP %d", ErrRejected, response.StatusCode)
	}

	return data, nil
}

func transportError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return context.DeadlineExceeded
	}

	return fmt.Errorf("%w: transport failed", ErrUnavailable)
}

func decodeResponse(data []byte, target any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: response is not UTF-8", ErrResponse)
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: expected a JSON object", ErrResponse)
	}

	if err := json.Unmarshal(trimmed, target); err != nil {
		return fmt.Errorf("%w: invalid JSON structure", ErrResponse)
	}

	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return fmt.Errorf("%w: invalid response envelope", ErrResponse)
	}
	if len(envelope.Error) != 0 {
		return fmt.Errorf("%w: unexpected error envelope", ErrResponse)
	}

	return nil
}
