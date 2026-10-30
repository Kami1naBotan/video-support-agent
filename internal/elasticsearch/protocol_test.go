package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/config"
)

const (
	protocolIndex  = "test_new_index"
	protocolOld    = "test_old_index"
	protocolAlias  = "test_rules_current"
	protocolSecret = "private-response-must-not-leak"
)

type protocolTransport func(*http.Request) (*http.Response, error)

func (f protocolTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type protocolStep struct {
	method string
	path   string
	status int
	body   string
	err    error
	check  func(*http.Request)
}

func protocolClient(t *testing.T, address string) *Client {
	t.Helper()

	client, err := NewClient(config.ElasticsearchConfig{
		URL:        address,
		IndexAlias: protocolAlias,
		Timeout:    "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func protocolScript(t *testing.T, steps ...protocolStep) *Client {
	t.Helper()

	client := protocolClient(t, "http://127.0.0.1:1")
	position := 0

	client.httpClient.Transport = protocolTransport(
		func(r *http.Request) (*http.Response, error) {
			if position >= len(steps) {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			}
			step := steps[position]
			position++

			if r.Method != step.method || r.URL.RequestURI() != step.path {
				t.Fatalf("request %d: want %s %s, got %s %s",
					position, step.method, step.path,
					r.Method, r.URL.RequestURI())
			}
			if r.Header.Get("Accept") != "application/json" {
				t.Fatal("missing Accept header")
			}
			if step.check != nil {
				step.check(r)
			}
			if step.err != nil {
				return nil, step.err
			}

			status := step.status
			if status == 0 {
				status = http.StatusOK
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(step.body)),
				Request:    r,
			}, nil
		},
	)

	t.Cleanup(func() {
		if position != len(steps) {
			t.Errorf("expected %d requests, received %d", len(steps), position)
		}
	})
	return client
}

func protocolJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func protocolAliasBody(index string) string {
	return `{"` + index + `":{"aliases":{"` + protocolAlias +
		`":{"is_write_index":false}}}}`
}

func protocolUnaliasedStep() protocolStep {
	return protocolStep{
		method: http.MethodGet,
		path:   "/" + protocolIndex + "/_alias",
		body:   `{"` + protocolIndex + `":{"aliases":{}}}`,
	}
}

func requireProtocolError(t *testing.T, err error, targets ...error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, target := range targets {
		if !errors.Is(err, target) {
			t.Fatalf("expected error class %v, got %v", target, err)
		}
	}
	if strings.Contains(err.Error(), protocolSecret) {
		t.Fatal("raw response or transport error leaked")
	}
}

type protocolBody struct {
	io.Reader
	closed bool
}

func (b *protocolBody) Close() error {
	b.closed = true
	return nil
}

type protocolBrokenReader struct{}

func (protocolBrokenReader) Read([]byte) (int, error) {
	return 0, errors.New(protocolSecret)
}

func TestESProtocolRequestBoundsAndClose(t *testing.T) {
	cases := []struct {
		name   string
		reader io.Reader
		want   error
		size   int
	}{
		{"normal", strings.NewReader("{}"), nil, 2},
		{"exact_limit", strings.NewReader(strings.Repeat("x", MaxResponseBytes)),
			nil, MaxResponseBytes},
		{"oversized", strings.NewReader(strings.Repeat("x", MaxResponseBytes+1)),
			ErrResponse, 0},
		{"read_failure", protocolBrokenReader{}, ErrUnavailable, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := protocolClient(t, "http://127.0.0.1:1")
			body := &protocolBody{Reader: tc.reader}
			client.httpClient.Transport = protocolTransport(
				func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Header:     make(http.Header),
						Body:       body,
						Request:    r,
					}, nil
				},
			)

			_, data, err := client.request(
				context.Background(), http.MethodGet, "/probe", "", nil,
			)
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if tc.want != nil {
				requireProtocolError(t, err, tc.want)
				if data != nil {
					t.Fatal("failed read returned partial data")
				}
			} else if err != nil || len(data) != tc.size {
				t.Fatalf("unexpected result: bytes=%d error=%v", len(data), err)
			}
		})
	}

	t.Run("request_overflow_before_transport", func(t *testing.T) {
		client := protocolScript(t)
		_, _, err := client.request(
			context.Background(), http.MethodPost, "/probe",
			"application/json", make([]byte, MaxRequestBytes+1),
		)
		requireProtocolError(t, err, ErrRejected)
	})

	t.Run("request_exact_limit", func(t *testing.T) {
		client := protocolScript(t, protocolStep{
			method: http.MethodPost,
			path:   "/probe",
			body:   "{}",
			check: func(r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil || len(data) != MaxRequestBytes {
					t.Fatal("exact request limit changed")
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("missing Content-Type")
				}
			},
		})
		_, _, err := client.request(
			context.Background(), http.MethodPost, "/probe",
			"application/json", make([]byte, MaxRequestBytes),
		)
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("canceled_before_transport", func(t *testing.T) {
		client := protocolScript(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := client.request(ctx, http.MethodGet, "/probe", "", nil)
		requireProtocolError(t, err, context.Canceled)
	})

	t.Run("transport_failure_is_sanitized", func(t *testing.T) {
		client := protocolScript(t, protocolStep{
			method: http.MethodGet,
			path:   "/probe",
			err:    errors.New(protocolSecret),
		})
		_, _, err := client.request(
			context.Background(), http.MethodGet, "/probe", "", nil,
		)
		requireProtocolError(t, err, ErrUnavailable)
	})
}

func TestESProtocolRealHTTP(t *testing.T) {
	t.Run("constructor_and_redirect", func(t *testing.T) {
		var calls atomic.Int32
		var followed atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/destination" {
					followed.Add(1)
					w.WriteHeader(200)
					return
				}
				w.Header().Set("Location", "/destination")
				w.WriteHeader(http.StatusTemporaryRedirect)
			},
		))
		t.Cleanup(server.Close)
		client := protocolClient(t, server.URL)

		if calls.Load() != 0 {
			t.Fatal("constructor performed network I/O")
		}
		status, _, err := client.request(
			context.Background(), http.MethodGet, "/redirect", "", nil,
		)
		if err != nil || status != http.StatusTemporaryRedirect ||
			calls.Load() != 1 || followed.Load() != 0 {
			t.Fatalf("redirect was followed or lost: status=%d error=%v", status, err)
		}
	})

	t.Run("client_timeout", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-release:
				}
			},
		))
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(release) })

		client := protocolClient(t, server.URL)
		client.httpClient.Timeout = 50 * time.Millisecond

		_, _, err := client.request(
			context.Background(), http.MethodGet, "/blocked", "", nil,
		)
		requireProtocolError(t, err, ErrUnavailable, context.DeadlineExceeded)
	})
}

func TestESProtocolAliasRead(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		target string
		want   error
	}{
		{"valid", 200, protocolAliasBody(protocolOld), protocolOld, nil},
		{"missing", 404, `{"error":"alias missing","status":404}`, "", nil},
		{"malformed_missing", 404, `{}`, "", ErrResponse},
		{"null_error", 404, `{"error":null,"status":404}`, "", ErrResponse},
		{"empty", 200, `{}`, "", ErrResponse},
		{"invalid_json", 200, `{`, "", ErrResponse},
		{"multiple", 200,
			`{"a":{"aliases":{}},"b":{"aliases":{}}}`, "", ErrResponse},
		{"writable", 200,
			`{"a":{"aliases":{"test_rules_current":{"is_write_index":true}}}}`,
			"", ErrResponse},
		{"implicit_write_setting", 200,
			`{"a":{"aliases":{"test_rules_current":{}}}}`, "", ErrResponse},
		{"filtered", 200,
			`{"a":{"aliases":{"test_rules_current":{"is_write_index":false,"filter":{}}}}}`,
			"", ErrResponse},
		{"routed", 200,
			`{"a":{"aliases":{"test_rules_current":{"is_write_index":false,"routing":"x"}}}}`,
			"", ErrResponse},
		{"unavailable_status", 503, protocolSecret, "", ErrRejected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := protocolScript(t, protocolStep{
				method: http.MethodGet,
				path:   "/_alias/" + protocolAlias,
				status: tc.status,
				body:   tc.body,
			})
			got, err := client.GetAliasTarget(context.Background())
			if tc.want != nil {
				requireProtocolError(t, err, tc.want)
				if got != "" {
					t.Fatal("invalid alias returned a target")
				}
			} else if err != nil || got != tc.target {
				t.Fatalf("target=%q error=%v", got, err)
			}
		})
	}
}

func TestESProtocolCreateIndex(t *testing.T) {
	definition := []byte(`{"settings":{},"mappings":{}}`)

	t.Run("invalid_inputs_make_no_request", func(t *testing.T) {
		client := protocolScript(t)
		for _, name := range []string{protocolAlias, "BadName", "a/b"} {
			requireProtocolError(t,
				client.CreateIndex(context.Background(), name, definition),
				ErrRejected)
		}
		for _, body := range []string{
			`null`, `{}`, `{"settings":[],"mappings":{}}`,
			`{"settings":{},"mappings":{},"aliases":{}}`,
		} {
			requireProtocolError(t,
				client.CreateIndex(context.Background(), protocolIndex, []byte(body)),
				ErrRejected)
		}
	})

	cases := []struct {
		name   string
		status int
		body   string
		cause  error
		want   error
	}{
		{"success", 200,
			`{"acknowledged":true,"shards_acknowledged":true,"index":"test_new_index"}`,
			nil, nil},
		{"wrong_identity", 200,
			`{"acknowledged":true,"shards_acknowledged":true,"index":"other"}`,
			nil, ErrStateUnknown},
		{"not_ready", 200,
			`{"acknowledged":true,"shards_acknowledged":false,"index":"test_new_index"}`,
			nil, ErrStateUnknown},
		{"invalid_json", 200, `{`, nil, ErrStateUnknown},
		{"rejected", 400, protocolSecret, nil, ErrRejected},
		{"server_failure", 503, protocolSecret, nil, ErrStateUnknown},
		{"timeout", 0, "", context.DeadlineExceeded, ErrStateUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := protocolScript(t, protocolStep{
				method: http.MethodPut,
				path:   "/" + protocolIndex + "?wait_for_active_shards=1",
				status: tc.status,
				body:   tc.body,
				err:    tc.cause,
				check: func(r *http.Request) {
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != string(definition) {
						t.Fatal("index definition changed")
					}
				},
			})
			err := client.CreateIndex(context.Background(), protocolIndex, definition)
			if tc.want != nil {
				requireProtocolError(t, err, tc.want)
				if tc.cause != nil {
					requireProtocolError(t, err, tc.cause)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
