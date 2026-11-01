package milvus

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

var (
	ErrInput        = errors.New("invalid milvus input")
	ErrUnavailable  = errors.New("milvus unavailable")
	ErrResponse     = errors.New("invalid milvus response")
	ErrRejected     = errors.New("milvus operation rejected")
	ErrStateUnknown = errors.New("milvus operation state unknown")
)

type APIError struct {
	Code int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("milvus operation rejected: code=%d", e.Code)
}

func (e *APIError) Unwrap() error {
	return ErrRejected
}

type Client struct {
	baseURL   string
	database  string
	alias     string
	timeout   time.Duration
	http      *http.Client
	transport *http.Transport
}

func NewClient(cfg config.MilvusConfig) (*Client, error) {
	if err := config.ValidateMilvusConfig(cfg); err != nil {
		return nil, err
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil {
		return nil, errors.New("invalid MILVUS_TIMEOUT")
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}

	return &Client{
		baseURL:  strings.TrimRight(cfg.URL, "/"),
		database: cfg.Database,
		alias:    cfg.CollectionAlias,
		timeout:  timeout,
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(
				req *http.Request,
				via []*http.Request,
			) error {
				return http.ErrUseLastResponse
			},
		},
		transport: transport,
	}, nil
}

func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}

func (c *Client) request(ctx context.Context, path string, payload map[string]any, mutation bool) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		body[key] = value
	}
	body["dbName"] = c.database

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode request", ErrInput)
	}
	if len(encoded) > maxRequestBytes {
		return nil, fmt.Errorf("%w: request exceeds byte budget", ErrInput)
	}

	req, err := http.NewRequestWithContext(opCtx, http.MethodPost, c.baseURL+"/v2/vectordb/"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", ErrInput)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if err := opCtx.Err(); err != nil {
		return nil, err
	}

	response, err := c.http.Do(req)
	if err != nil {
		return nil, operationError(mutation, transportError(opCtx, err))
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, int64(maxResponseBytes)+1))
	if err != nil {
		return nil, operationError(mutation, transportError(opCtx, err))
	}
	if err := opCtx.Err(); err != nil {
		return nil, operationError(mutation, err)
	}
	if len(data) > maxResponseBytes {
		return nil, operationError(mutation, fmt.Errorf("%w: response exceeds byte budget", ErrResponse))
	}

	if response.StatusCode != http.StatusOK {
		kind := ErrRejected
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout {
			kind = ErrUnavailable
		}
		return nil, operationError(mutation, fmt.Errorf("%w: HTTP %d", kind, response.StatusCode))
	}

	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if !utf8.Valid(data) ||
		json.Unmarshal(data, &envelope) != nil ||
		envelope.Code == nil {
		return nil, operationError(mutation, fmt.Errorf("%w: invalid response envelope", ErrResponse))
	}

	if *envelope.Code != 0 {
		return nil, operationError(mutation, &APIError{Code: *envelope.Code})
	}

	trimmed := bytes.TrimSpace(envelope.Data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, operationError(mutation, fmt.Errorf("%w: missing response data", ErrResponse))
	}

	return envelope.Data, nil
}

func operationError(mutation bool, err error) error {
	if !mutation {
		return err
	}
	return fmt.Errorf("%w: %w", ErrStateUnknown, err)
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
