package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/vectorrebuild"
)

const cliSecret = "PRIVATE_TEST_SECRET_92741"

func cliArgs() []string {
	return []string{"-expect-db", "test_source", "-expect-server-uuid", "test-server"}
}

func cliEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DB_HOST": "127.0.0.1", "DB_PORT": "1",
		"DB_USER": "test_user", "DB_PASSWORD": cliSecret, "DB_NAME": "test_source",
		"EMBEDDING_URL":            "http://127.0.0.1:1",
		"EMBEDDING_MODEL":          "bge-m3:567m",
		"EMBEDDING_MODEL_DIGEST":   strings.Repeat("a", 64),
		"EMBEDDING_DIMENSIONS":     "1024",
		"EMBEDDING_CONTEXT_LENGTH": "8192",
		"EMBEDDING_TIMEOUT":        "2s",
		"MILVUS_URL":               "http://127.0.0.1:1",
		"MILVUS_DATABASE":          "default",
		"MILVUS_COLLECTION_ALIAS":  "vector_cli_test",
		"MILVUS_TIMEOUT":           "2s",
		// 这些配置不是重建CLI的前置条件。
		"JWT_SECRET": "", "JWT_TTL": "invalid",
		"LLM_API_KEY": "", "LLM_TIMEOUT": "invalid",
		"REDIS_ADDR": "", "ES_URL": "invalid",
		"TRUSTED_PROXIES": "invalid",
	} {
		t.Setenv(key, value)
	}
}

func cliSingleEvent(t *testing.T, raw []byte) event {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var result event
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("invalid JSON event: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected exactly one event, got %v", err)
	}
	if result.Time.IsZero() {
		t.Fatal("event missing timestamp")
	}
	return result
}

func TestVectorCLIOptions(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		opts, err := parseOptions(cliArgs(), io.Discard)
		if err != nil || opts.expectedDatabase != "test_source" ||
			opts.expectedServerUUID != "test-server" ||
			opts.timeout != 15*time.Minute || opts.milvusTimeout != 0 {
			t.Fatalf("wrong defaults: %+v %v", opts, err)
		}
	})
	t.Run("overrides", func(t *testing.T) {
		args := append(cliArgs(), "-timeout", "2m", "-milvus-timeout", "7s")
		opts, err := parseOptions(args, io.Discard)
		if err != nil || opts.timeout != 2*time.Minute || opts.milvusTimeout != 7*time.Second {
			t.Fatalf("wrong overrides: %+v %v", opts, err)
		}
	})
	t.Run("help", func(t *testing.T) {
		var help bytes.Buffer
		_, err := parseOptions([]string{"-h"}, &help)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatal(err)
		}
		for _, text := range []string{
			"Usage: vector-reindex", "-expect-db", "-expect-server-uuid",
			"-timeout", "-milvus-timeout",
		} {
			if !strings.Contains(help.String(), text) {
				t.Fatalf("missing help text %s", text)
			}
		}
	})

	for _, tt := range []struct {
		name string
		args []string
	}{
		{"missing", nil},
		{"missing_uuid", []string{"-expect-db", "test_source"}},
		{"blank_db", []string{"-expect-db", " ", "-expect-server-uuid", "test-server"}},
		{"spaced_uuid", []string{"-expect-db", "test_source", "-expect-server-uuid", " server "}},
		{"invalid_utf8", []string{"-expect-db", string([]byte{0xff}), "-expect-server-uuid", "server"}},
		{"unknown", append(cliArgs(), "-"+cliSecret)},
		{"positional", append(cliArgs(), cliSecret)},
		{"zero_timeout", append(cliArgs(), "-timeout", "0s")},
		{"negative_timeout", append(cliArgs(), "-timeout", "-1s")},
		{"bad_timeout", append(cliArgs(), "-timeout", cliSecret)},
		{"negative_milvus_timeout", append(cliArgs(), "-milvus-timeout", "-1s")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseOptions(tt.args, &output)
			if err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if strings.Contains(err.Error()+output.String(), cliSecret) {
				t.Fatal("argument error exposed raw input")
			}
		})
	}
}

func TestVectorCLIEarlyExit(t *testing.T) {
	for _, mode := range []string{"help", "arguments", "embedding", "milvus", "database"} {
		t.Run(mode, func(t *testing.T) {
			cliEnvironment(t)
			args := cliArgs()
			stage := "configuration"
			switch mode {
			case "help":
				args = []string{"-h"}
				t.Setenv("EMBEDDING_MODEL", "invalid")
			case "arguments":
				args = []string{"-" + cliSecret}
				stage = "arguments"
			case "embedding":
				t.Setenv("EMBEDDING_MODEL_DIGEST", cliSecret)
			case "milvus":
				t.Setenv("MILVUS_URL", "http://user:"+cliSecret+"@localhost")
			case "database":
				t.Setenv("DB_NAME", cliSecret)
			}
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if strings.Contains(stdout.String()+stderr.String(), cliSecret) {
				t.Fatal("CLI output leaked raw configuration or arguments")
			}
			if mode == "help" {
				if code != 0 || stdout.Len() != 0 ||
					!strings.Contains(stderr.String(), "Usage: vector-reindex") {
					t.Fatal("help did not exit before configuration")
				}
				return
			}
			if code != 2 {
				t.Fatalf("exit=%d, want=2", code)
			}
			got := cliSingleEvent(t, stdout.Bytes())
			if got.Event != "finished" || got.Stage != stage ||
				got.ExitCode == nil || *got.ExitCode != code ||
				got.NewCollection != "" || got.Result != nil ||
				len(got.ErrorClasses) == 0 {
				t.Fatalf("incorrect early-exit event: %+v", got)
			}
		})
	}
}

type cliFailWriter struct {
	attempted bytes.Buffer
	calls     int
}

func (w *cliFailWriter) Write(p []byte) (int, error) {
	w.calls++
	_, _ = w.attempted.Write(p)
	return 0, errors.New(cliSecret)
}

func TestVectorCLIOutputFailure(t *testing.T) {
	for _, mode := range []string{"final", "start", "timeout_override"} {
		t.Run(mode, func(t *testing.T) {
			cliEnvironment(t)
			args := cliArgs()
			if mode == "final" {
				args = nil
			}
			if mode == "timeout_override" {
				// 有效命令行覆盖必须替换无效环境超时。
				t.Setenv("MILVUS_TIMEOUT", "invalid")
				args = append(args, "-milvus-timeout", "3s")
			}
			stdout := &cliFailWriter{}
			var stderr bytes.Buffer
			code := run(args, stdout, &stderr)
			if code != 1 || stdout.calls != 1 {
				t.Fatalf("output failure: exit=%d writes=%d", code, stdout.calls)
			}
			got := cliSingleEvent(t, stdout.attempted.Bytes())
			if mode == "final" {
				if got.Event != "finished" || got.Stage != "arguments" ||
					!strings.Contains(stderr.String(), "Cannot write final evidence") {
					t.Fatal("final evidence failure was not reported")
				}
			} else {
				if got.Event != "started" || got.Stage != "connect" ||
					!strings.HasPrefix(got.NewCollection, "rule_chunks_vec_v1_") ||
					!strings.Contains(stderr.String(), "Cannot write start evidence") {
					t.Fatal("start failure did not stop before database connection")
				}
			}
			if strings.Contains(stderr.String(), cliSecret) ||
				strings.Contains(stdout.attempted.String(), cliSecret) {
				t.Fatal("writer error or credentials leaked")
			}
		})
	}
}

func TestVectorCLIErrorClasses(t *testing.T) {
	cases := []struct {
		err  error
		name string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{indexing.ErrInvalidSource, "invalid_source"},
		{indexing.ErrExportLimitExceeded, "export_limit_exceeded"},
		{embedding.ErrInput, "embedding_input"},
		{embedding.ErrInputTooLarge, "embedding_input_too_large"},
		{embedding.ErrContextLength, "embedding_context_length"},
		{embedding.ErrUnavailable, "embedding_unavailable"},
		{embedding.ErrRejected, "embedding_rejected"},
		{embedding.ErrResponse, "embedding_response_invalid"},
		{embedding.ErrModelMismatch, "model_mismatch"},
		{milvus.ErrInput, "milvus_input"},
		{milvus.ErrUnavailable, "milvus_unavailable"},
		{milvus.ErrResponse, "milvus_response_invalid"},
		{milvus.ErrRejected, "milvus_rejected"},
		{milvus.ErrStateUnknown, "milvus_state_unknown"},
		{vectorrebuild.ErrEvidence, "evidence_write_failed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyError(fmt.Errorf("%s: %w", cliSecret, tt.err))
			if !reflect.DeepEqual(got, []string{tt.name}) {
				t.Fatalf("classes=%v", got)
			}
		})
	}
	t.Run("joined", func(t *testing.T) {
		err := errors.Join(milvus.ErrStateUnknown, context.DeadlineExceeded)
		want := []string{"deadline_exceeded", "milvus_state_unknown"}
		if !reflect.DeepEqual(classifyError(err), want) {
			t.Fatal("joined error lost a classification")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		got := classifyError(errors.New(cliSecret))
		if !reflect.DeepEqual(got, []string{"operation_failed"}) {
			t.Fatal("unknown error exposed details")
		}
	})
	t.Run("nil", func(t *testing.T) {
		if classifyError(nil) != nil {
			t.Fatal("successful result contains error classes")
		}
	})
}

func TestVectorCLIDatabase(t *testing.T) {
	baseline := config.DatabaseConfig{
		Host: "127.0.0.1", Port: "3307",
		User: "test_user", Password: cliSecret, Name: "test_source",
	}
	if err := validateDatabaseConfig(baseline, "test_source"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{
		"host", "user", "password", "name", "port_zero",
		"port_large", "port_text", "identity",
	} {
		t.Run(mode, func(t *testing.T) {
			cfg := baseline
			expected := "test_source"
			switch mode {
			case "host":
				cfg.Host = " "
			case "user":
				cfg.User = " "
			case "password":
				cfg.Password = ""
			case "name":
				cfg.Name = " "
			case "port_zero":
				cfg.Port = "0"
			case "port_large":
				cfg.Port = "65536"
			case "port_text":
				cfg.Port = cliSecret
			case "identity":
				expected = "another_db"
			}
			err := validateDatabaseConfig(cfg, expected)
			if err == nil || strings.Contains(err.Error(), cliSecret) {
				t.Fatal("invalid database configuration accepted or exposed")
			}
		})
	}
	t.Run("canceled_before_connect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		db, pool, err := openSourceDatabase(ctx, baseline)
		if !errors.Is(err, context.Canceled) || db != nil || pool != nil {
			t.Fatal("canceled database initialization did not stop")
		}
	})
}
