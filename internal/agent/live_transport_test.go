//go:build agentlive

package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type liveWireBody struct {
	io.Reader
	io.Closer
}

type liveWireTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	rows []map[string]any
}

func liveWireJSON(data []byte, fields ...string) map[string]json.RawMessage {
	var source map[string]json.RawMessage
	if json.Unmarshal(data, &source) != nil {
		return nil
	}
	result := make(map[string]json.RawMessage)
	for _, field := range fields {
		if value, ok := source[field]; ok {
			result[field] = value
		}
	}
	return result
}

func (w *liveWireTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	row := map[string]any{
		"Method": req.Method,
		"Host":   req.URL.Host,
		"Path":   req.URL.Path,
	}
	// 读取独立副本；不读取Authorization等请求头，不修改真实请求。
	if req.GetBody != nil {
		copyBody, err := req.GetBody()
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(copyBody, 1024*1024+1))
			copyBody.Close()
			if readErr == nil && len(data) <= 1024*1024 {
				row["Request"] = liveWireJSON(data,
					"model", "messages", "tools", "tool_choice", "parallel_tool_calls",
					"temperature", "top_p", "max_tokens", "stream", "reasoning_effort")
			} else {
				row["RequestCapture"] = "unavailable_or_too_large"
			}
		} else {
			row["RequestCapture"] = "copy_failed"
		}
	} else {
		row["RequestCapture"] = "no_body_copy"
	}

	response, err := w.base.RoundTrip(req)
	if err != nil {
		row["TransportError"] = liveError(err)
	}
	if response != nil {
		row["StatusCode"] = response.StatusCode
		if response.Body != nil {
			original := response.Body
			data, readErr := io.ReadAll(io.LimitReader(original, 1024*1024+1))
			// 已读取的字节原样放回，余下字节继续由原Body提供。
			// 原始读取错误也交回调用者，不把读取失败伪装成EOF。
			response.Body = &liveWireBody{
				Reader: io.MultiReader(bytes.NewReader(data), &liveWireTail{Reader: original, err: readErr}),
				Closer: original,
			}
			if readErr == nil && len(data) <= 1024*1024 && response.StatusCode == http.StatusOK {
				projected := liveWireJSON(data, "model", "id")
				var payload struct {
					Choices []struct {
						Index        int             `json:"index"`
						FinishReason string          `json:"finish_reason"`
						Message      json.RawMessage `json:"message"`
					} `json:"choices"`
				}
				if projected != nil && json.Unmarshal(data, &payload) == nil {
					choices := make([]map[string]any, 0, len(payload.Choices))
					for _, c := range payload.Choices {
						choices = append(choices, map[string]any{
							"index": c.Index, "finish_reason": c.FinishReason,
							"message": liveWireJSON(c.Message, "role", "content", "tool_calls"),
						})
					}
					row["Response"] = map[string]any{"metadata": projected, "choices": choices}
				} else {
					row["ResponseCapture"] = "invalid_json"
				}
			} else {
				row["ResponseCapture"] = "not_success_or_unavailable"
			}
		}
	}
	w.mu.Lock()
	w.rows = append(w.rows, row)
	w.mu.Unlock()
	return response, err
}

type liveWireTail struct {
	io.Reader
	err error
}

func (r *liveWireTail) Read(p []byte) (int, error) {
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return r.Reader.Read(p)
}

func traceLiveHTTP(t *testing.T, outputDir string) {
	t.Helper()
	previous := http.DefaultTransport
	recorder := &liveWireTransport{base: previous, rows: []map[string]any{}}
	http.DefaultTransport = recorder
	t.Cleanup(func() {
		http.DefaultTransport = previous
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		data, err := json.MarshalIndent(recorder.rows, "", "  ")
		if err != nil {
			t.Error("encode HTTP evidence")
			return
		}
		if err := os.WriteFile(filepath.Join(outputDir, "http.json"), data, 0600); err != nil {
			t.Error("save HTTP evidence")
		}
		if len(recorder.rows) == 0 {
			t.Error("HTTP transport was not observed")
		}
	})
}
