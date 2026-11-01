package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"video-support-agent/internal/config"
)

const protocolSecret = "PRIVATE_RULE_CONTENT_92741"

func protocolConfig(url string) config.EmbeddingConfig {
	return config.EmbeddingConfig{
		URL:           url,
		Model:         ModelName,
		ModelDigest:   strings.Repeat("a", 64),
		Dimensions:    "1024",
		ContextLength: "8192",
		Timeout:       "5s",
	}
}

func protocolClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := NewClient(protocolConfig(url))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func protocolJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func protocolEmbedding(t *testing.T, vectors any) string {
	t.Helper()
	return protocolJSON(t, map[string]any{
		"model": ModelName, "embeddings": vectors,
	})
}

func protocolReplies(t *testing.T, count int) []string {
	t.Helper()
	tags := protocolJSON(t, map[string]any{
		"models": []any{map[string]any{
			"name": ModelName, "digest": strings.Repeat("a", 64),
		}},
	})
	show := `{"capabilities":["embedding"],"model_info":` +
		`{"bert.embedding_length":1024,"bert.context_length":8192}}`
	version := `{"version":"0.40.13"}`
	vectors := make([][]float64, count)
	for i := range vectors {
		vectors[i] = contractVector(float64(i + 1))
	}
	return []string{
		tags, show, version, protocolEmbedding(t, vectors),
		tags, show, version,
	}
}

var protocolPaths = []string{
	"/api/tags", "/api/show", "/api/version", "/api/embed",
	"/api/tags", "/api/show", "/api/version",
}

func checkProtocolRequest(t *testing.T, req *http.Request, index int, texts []string) {
	t.Helper()
	method := http.MethodGet
	if index == 1 || index == 3 || index == 5 {
		method = http.MethodPost
	}
	if req.Method != method || req.URL.Path != protocolPaths[index] {
		t.Errorf("request %d: %s %s", index, req.Method, req.URL.Path)
	}
	if req.URL.RawQuery != "" || req.Header.Get("Accept") != "application/json" {
		t.Error("unexpected query or Accept header")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
		return
	}
	if method == http.MethodGet {
		if len(body) != 0 {
			t.Error("GET request unexpectedly contains a body")
		}
		return
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Error("missing JSON Content-Type")
	}
	if index != 3 {
		var show struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &show) != nil || show.Model != ModelName {
			t.Error("invalid show request")
		}
		return
	}
	var embed struct {
		Model     string   `json:"model"`
		Input     []string `json:"input"`
		Truncate  *bool    `json:"truncate"`
		KeepAlive string   `json:"keep_alive"`
		Options   struct {
			NumCtx int `json:"num_ctx"`
		} `json:"options"`
	}
	if err := json.Unmarshal(body, &embed); err != nil {
		t.Errorf("decode embed request: %v", err)
		return
	}
	if embed.Model != ModelName ||
		!reflect.DeepEqual(embed.Input, texts) ||
		embed.Truncate == nil || *embed.Truncate ||
		embed.KeepAlive != "5m" || embed.Options.NumCtx != 8192 {
		t.Error("embed request violated model/input/truncation/context contract")
	}
}

// 顺序和请求次数同时断言，失败后多请求或自动重试都会被发现。
func protocolServer(
	t *testing.T, texts []string, replies []string, failAt, status int,
) (*Client, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(replies) {
			t.Error("unexpected extra request")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		checkProtocolRequest(t, r, i, texts)
		w.Header().Set("Content-Type", "application/json")
		if i == failAt {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, replies[i])
	}))
	t.Cleanup(server.Close)
	return protocolClient(t, server.URL), calls
}

func requireProtocolFailure(t *testing.T, result BatchResult, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if result.Vectors != nil || result.Identity != (ModelIdentity{}) {
		t.Fatal("failure returned vectors or a usable identity")
	}
	if strings.Contains(err.Error(), protocolSecret) {
		t.Fatal("error exposed private text")
	}
}

func TestOllamaProtocol(t *testing.T) {
	for _, wrapper := range []bool{false, true} {
		name := "with_identity"
		if wrapper {
			name = "embed_interface"
		}
		t.Run(name, func(t *testing.T) {
			texts := []string{" \t第一条规则\r\n", "第二条规则"}
			client, calls := protocolServer(
				t, texts, protocolReplies(t, len(texts)), -1, 200,
			)
			if calls.Load() != 0 {
				t.Fatal("constructor performed network I/O")
			}

			var vectors [][]float32
			if wrapper {
				var err error
				vectors, err = client.Embed(context.Background(), texts)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				got, err := client.EmbedWithIdentity(context.Background(), texts)
				if err != nil {
					t.Fatal(err)
				}
				want := ModelIdentity{
					Model: ModelName, Digest: strings.Repeat("a", 64),
					ServerVersion: "0.40.13", Dimensions: 1024,
					ContextLength: 8192, InputPolicy: InputPolicy,
				}
				if got.Identity != want {
					t.Fatalf("identity = %+v", got.Identity)
				}
				vectors = got.Vectors
			}
			if calls.Load() != 7 {
				t.Fatalf("requests = %d, want 7", calls.Load())
			}
			if len(vectors) != 2 {
				t.Fatal("wrong vector count")
			}
			for i, vector := range vectors {
				if len(vector) != 1024 || vector[0] != float32(i+1) {
					t.Fatalf("wrong shape/order at vector %d", i)
				}
				for _, component := range vector[1:] {
					if component != 0 {
						t.Fatal("unexpected vector component")
					}
				}
			}
		})
	}
}

func TestOllamaFailures(t *testing.T) {
	valid := protocolReplies(t, 1)
	nullVector := make([]any, Dimensions)
	for i := range nullVector {
		nullVector[i] = float64(0)
	}
	nullVector[0] = float64(1)
	nullVector[Dimensions-1] = nil

	show := func(capability string, dimensions, contextLength any) string {
		return protocolJSON(t, map[string]any{
			"capabilities": []string{capability},
			"model_info": map[string]any{
				"bert.embedding_length": dimensions,
				"bert.context_length":   contextLength,
			},
		})
	}
	tests := []struct {
		name   string
		at     int
		status int
		body   string
		want   error
	}{
		{"tags_missing", 0, 200, `{}`, ErrResponse},
		{"model_missing", 0, 200, `{"models":[]}`, ErrModelMismatch},
		{"wrong_digest", 0, 200,
			strings.ReplaceAll(valid[0], strings.Repeat("a", 64), strings.Repeat("b", 64)),
			ErrModelMismatch},
		{"duplicate_model", 0, 200,
			protocolJSON(t, map[string]any{
				"models": []any{
					map[string]any{
						"name":   ModelName,
						"digest": strings.Repeat("a", 64),
					},
					map[string]any{
						"name":   ModelName,
						"digest": strings.Repeat("a", 64),
					},
				},
			}),
			ErrModelMismatch},
		{"metadata_missing", 1, 200, `{}`, ErrResponse},
		{"dimension_null", 1, 200, show("embedding", nil, 8192), ErrResponse},
		{"dimension_wrong", 1, 200, show("embedding", 768, 8192), ErrModelMismatch},
		{"context_wrong", 1, 200, show("embedding", 1024, 4096), ErrModelMismatch},
		{"capability_wrong", 1, 200, show("completion", 1024, 8192), ErrModelMismatch},
		{"metadata_type", 1, 200, show("embedding", "1024", 8192), ErrResponse},
		{"version_blank", 2, 200, `{"version":""}`, ErrResponse},
		{"version_whitespace", 2, 200, `{"version":" 0.40.13"}`, ErrResponse},
		{"response_model", 3, 200,
			strings.ReplaceAll(valid[3], ModelName, "other-model"), ErrModelMismatch},
		{"missing_vectors", 3, 200, `{"model":"bge-m3:567m"}`, ErrResponse},
		{"wrong_count", 3, 200, protocolEmbedding(t, [][]float64{}), ErrResponse},
		{"short_vector", 3, 200,
			protocolEmbedding(t, [][]float64{make([]float64, 1023)}), ErrResponse},
		{"null_vector", 3, 200,
			protocolEmbedding(t, []any{nil}), ErrResponse},
		{"null_component", 3, 200,
			protocolEmbedding(t, []any{nullVector}), ErrResponse},
		{"zero_vector", 3, 200,
			protocolEmbedding(t, [][]float64{contractVector(0)}), ErrResponse},
		{"float32_overflow", 3, 200,
			protocolEmbedding(t, [][]float64{contractVector(1e39)}), ErrResponse},
		{"empty_response", 3, 200, "", ErrResponse},
		{"array_response", 3, 200, `[]`, ErrResponse},
		{"null_response", 3, 200, `null`, ErrResponse},
		{"trailing_json", 3, 200, valid[3] + `{}`, ErrResponse},
		{"invalid_utf8", 3, 200, string([]byte{0xff}), ErrResponse},
		{"error_envelope", 3, 200,
			`{"error":"` + protocolSecret + `"}`, ErrResponse},
		{"null_error_envelope", 3, 200, `{"error":null}`, ErrResponse},
		{"context_length", 3, 400,
			`{"error":"the input length exceeds the context length"}`, ErrContextLength},
		{"other_bad_request", 3, 400,
			`{"error":"` + protocolSecret + `"}`, ErrRejected},
		{"context_message_wrong_endpoint", 1, 400,
			`{"error":"the input length exceeds the context length"}`, ErrRejected},
		{"unauthorized", 3, 401, protocolSecret, ErrRejected},
		{"rate_limited", 3, 429, protocolSecret, ErrUnavailable},
		{"request_timeout", 3, 408, protocolSecret, ErrUnavailable},
		{"server_error", 3, 503, protocolSecret, ErrUnavailable},
		{"digest_changed_after_embed", 4, 200,
			strings.ReplaceAll(valid[4], strings.Repeat("a", 64), strings.Repeat("b", 64)),
			ErrModelMismatch},
		{"metadata_changed_after_embed", 5, 200,
			show("embedding", 768, 8192), ErrModelMismatch},
		{"version_changed_after_embed", 6, 200,
			`{"version":"0.40.14"}`, ErrModelMismatch},
		{"final_identity_unavailable", 6, 503, protocolSecret, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replies := append([]string(nil), valid...)
			replies[tt.at] = tt.body
			client, calls := protocolServer(
				t, []string{protocolSecret}, replies, tt.at, tt.status,
			)
			got, err := client.EmbedWithIdentity(
				context.Background(), []string{protocolSecret},
			)
			requireProtocolFailure(t, got, err, tt.want)
			if calls.Load() != int32(tt.at+1) {
				t.Fatalf("requests = %d, want %d; unexpected retry/continuation",
					calls.Load(), tt.at+1)
			}
		})
	}
}

type protocolTransport func(*http.Request) (*http.Response, error)

func (f protocolTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOllamaPreflight(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  error
	}{
		{"empty", nil, ErrInput},
		{"too_large", []string{strings.Repeat("x", MaxTextBytes+1)}, ErrInputTooLarge},
		{"canceled", []string{"规则"}, context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := protocolClient(t, "http://127.0.0.1:1")
			calls := 0
			client.http.Transport = protocolTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected transport call")
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.name == "canceled" {
				cancel()
			}
			got, err := client.EmbedWithIdentity(ctx, tt.texts)
			requireProtocolFailure(t, got, err, tt.want)
			vectors, err := client.Embed(ctx, tt.texts)
			if vectors != nil || !errors.Is(err, tt.want) {
				t.Fatal("Embed wrapper lost failure contract")
			}
			if calls != 0 {
				t.Fatal("invalid input or canceled operation reached transport")
			}
		})
	}
}

func TestOllamaContext(t *testing.T) {
	t.Run("shared_deadline", func(t *testing.T) {
		client := protocolClient(t, "http://127.0.0.1:1")
		client.timeout = time.Second
		replies := protocolReplies(t, 1)
		var first time.Time
		calls := 0
		client.http.Transport = protocolTransport(func(r *http.Request) (*http.Response, error) {
			i := calls
			calls++
			if i >= len(replies) {
				return nil, errors.New("unexpected extra call")
			}
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Error("request has no deadline")
			}
			if i == 0 {
				first = deadline
			} else if !deadline.Equal(first) {
				t.Error("deadline was reset between HTTP requests")
			}
			return &http.Response{
				StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(replies[i])),
			}, nil
		})
		start := time.Now()
		_, err := client.EmbedWithIdentity(context.Background(), []string{"规则"})
		if err != nil || calls != 7 {
			t.Fatalf("calls=%d error=%v", calls, err)
		}
		if first.Before(start) || first.After(time.Now().Add(time.Second)) {
			t.Fatal("operation deadline does not respect configured budget")
		}
	})

	for _, mode := range []string{"configured_timeout", "parent_timeout", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			client := protocolClient(t, "http://127.0.0.1:1")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var parentDeadline time.Time
			if mode == "configured_timeout" {
				client.timeout = 25 * time.Millisecond
			}
			if mode == "parent_timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 25*time.Millisecond)
				defer stop()
				parentDeadline, _ = ctx.Deadline()
			}
			calls := 0
			client.http.Transport = protocolTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if mode == "parent_timeout" {
					d, ok := r.Context().Deadline()
					if !ok || !d.Equal(parentDeadline) {
						t.Error("shorter parent deadline was not preserved")
					}
				}
				if mode == "parent_cancel" {
					cancel()
				}
				// 等待实际上下文终止，不用 Sleep 猜测超时何时发生。
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			got, err := client.EmbedWithIdentity(ctx, []string{"规则"})
			want := context.DeadlineExceeded
			if mode == "parent_cancel" {
				want = context.Canceled
			}
			requireProtocolFailure(t, got, err, want)
			if calls != 1 {
				t.Fatalf("calls=%d, expected stop after first request", calls)
			}
		})
	}
}

type protocolBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *protocolBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *protocolBody) Close() error {
	b.closed = true
	return nil
}

type protocolBrokenReader struct{}

func (protocolBrokenReader) Read([]byte) (int, error) {
	return 0, errors.New(protocolSecret)
}

func TestOllamaRequestBoundaries(t *testing.T) {
	t.Run("request_encoding_and_budget", func(t *testing.T) {
		client := protocolClient(t, "http://127.0.0.1:1")
		calls := 0
		client.http.Transport = protocolTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{}`)),
			}, nil
		})
		for _, tt := range []struct {
			payload any
			want    error
		}{
			{make(chan int), ErrInput},
			{strings.Repeat("x", maxRequestBytes-1), ErrInputTooLarge},
		} {
			data, err := client.request(context.Background(), "POST", "/api/embed", tt.payload)
			if data != nil || !errors.Is(err, tt.want) {
				t.Fatalf("error=%v want=%v", err, tt.want)
			}
		}
		if calls != 0 {
			t.Fatal("invalid encoded payload reached transport")
		}
		// JSON 字符串含两侧引号：内容长度+2恰好等于预算。
		_, err := client.request(context.Background(), "POST", "/api/embed",
			strings.Repeat("x", maxRequestBytes-2))
		if err != nil || calls != 1 {
			t.Fatalf("exact request budget rejected: %v", err)
		}
	})

	for _, tt := range []struct {
		name   string
		reader io.Reader
		status int
		want   error
		size   int
	}{
		{"exact_response", strings.NewReader(strings.Repeat("x", maxResponseBytes)),
			200, nil, maxResponseBytes},
		{"oversized_response", strings.NewReader(strings.Repeat("x", maxResponseBytes+100)),
			200, ErrResponse, 0},
		{"read_error", protocolBrokenReader{}, 200, ErrUnavailable, 0},
		{"rejected_response", strings.NewReader(protocolSecret), 403, ErrRejected, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := protocolClient(t, "http://127.0.0.1:1")
			body := &protocolBody{reader: tt.reader}
			client.http.Transport = protocolTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.status, Header: make(http.Header), Body: body,
				}, nil
			})
			data, err := client.request(context.Background(), "GET", "/api/tags", nil)
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if body.read > maxResponseBytes+1 {
				t.Fatal("reader exceeded bounded response budget")
			}
			if tt.want == nil {
				if err != nil || len(data) != tt.size {
					t.Fatalf("length=%d error=%v", len(data), err)
				}
			} else {
				if !errors.Is(err, tt.want) || data != nil {
					t.Fatalf("error=%v want=%v", err, tt.want)
				}
				if strings.Contains(err.Error(), protocolSecret) {
					t.Fatal("error exposed response/transport details")
				}
			}
		})
	}

	t.Run("transport_failure_redacted", func(t *testing.T) {
		client := protocolClient(t, "http://127.0.0.1:1")
		calls := 0
		client.http.Transport = protocolTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New(protocolSecret)
		})
		got, err := client.EmbedWithIdentity(context.Background(), []string{protocolSecret})
		requireProtocolFailure(t, got, err, ErrUnavailable)
		if calls != 1 || strings.Contains(err.Error(), client.baseURL) {
			t.Fatal("transport retried or exposed endpoint")
		}
	})
}

func TestOllamaRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer target.Close()
	var sourceCalls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		http.Redirect(w, r, target.URL+"/private", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := protocolClient(t, source.URL)
	got, err := client.EmbedWithIdentity(context.Background(), []string{protocolSecret})
	requireProtocolFailure(t, got, err, ErrRejected)
	if sourceCalls.Load() != 1 || targetCalls.Load() != 0 {
		t.Fatal("redirect was followed or request was retried")
	}
}

func TestOllamaCloseIdle(t *testing.T) {
	idle := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			select {
			case idle <- struct{}{}:
			default:
			}
		case http.StateClosed:
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	client := protocolClient(t, server.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.request(ctx, "GET", "/api/tags", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("connection did not become idle")
	}
	client.Close()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("Close did not release the idle connection")
	}
}
