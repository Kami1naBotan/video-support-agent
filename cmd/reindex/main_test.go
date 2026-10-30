package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	indexing "video-support-agent/internal/index"
)

const cliTestSecret = "isolated-secret-must-not-appear"

func cliArguments() []string {
	return []string{
		"-expect-db", "isolated_rules",
		"-expect-server-uuid", "00000000-0000-0000-0000-000000000001",
	}
}

func cliEnvironment(t *testing.T) {
	t.Helper()

	values := map[string]string{
		"DB_HOST":        "127.0.0.1",
		"DB_PORT":        "3307",
		"DB_USER":        "isolated_user",
		"DB_PASSWORD":    cliTestSecret,
		"DB_NAME":        "isolated_rules",
		"ES_URL":         "http://127.0.0.1:9200",
		"ES_INDEX_ALIAS": "test_rules_current",
		"ES_TIMEOUT":     "5s",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func cliWriteMapping(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mapping.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReindexOptions(t *testing.T) {
	got, err := parseOptions(cliArguments(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.expectedDatabase != "isolated_rules" ||
		got.expectedServerUUID != "00000000-0000-0000-0000-000000000001" ||
		got.timeout != 5*time.Minute ||
		got.allowEmpty ||
		got.mappingPath != "deploy/elasticsearch/rule-chunks-v1.json" {
		t.Fatalf("incorrect defaults: %+v", got)
	}

	args := append(cliArguments(),
		"-allow-empty",
		"-timeout", "2m",
		"-mapping", "custom.json",
	)
	got, err = parseOptions(args, io.Discard)
	if err != nil || !got.allowEmpty ||
		got.timeout != 2*time.Minute || got.mappingPath != "custom.json" {
		t.Fatalf("explicit options were lost: %+v, %v", got, err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"missing_identity", nil},
		{"missing_uuid", []string{"-expect-db", "isolated_rules"}},
		{"whitespace", append(cliArguments(), "-expect-db", " isolated_rules")},
		{"zero_timeout", append(cliArguments(), "-timeout", "0s")},
		{"negative_timeout", append(cliArguments(), "-timeout", "-1s")},
		{"invalid_duration", append(cliArguments(), "-timeout", cliTestSecret)},
		{"invalid_bool", append(cliArguments(), "-allow-empty="+cliTestSecret)},
		{"unknown_option", append(cliArguments(), "-"+cliTestSecret)},
		{"positional", append(cliArguments(), cliTestSecret)},
		{"empty_mapping", append(cliArguments(), "-mapping", " ")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := parseOptions(tc.args, &output)
			if err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if strings.Contains(err.Error()+output.String(), cliTestSecret) {
				t.Fatal("argument values leaked through an error")
			}
		})
	}
}

func TestReindexDatabaseConfiguration(t *testing.T) {
	valid := config.DatabaseConfig{
		Host:     "127.0.0.1",
		Port:     "3307",
		User:     "isolated_user",
		Password: cliTestSecret,
		Name:     "isolated_rules",
	}
	opts := options{expectedDatabase: valid.Name}
	if err := validateDatabaseConfig(valid, opts); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		change func(*config.DatabaseConfig)
	}{
		{"host", func(c *config.DatabaseConfig) { c.Host = " " }},
		{"user", func(c *config.DatabaseConfig) { c.User = "" }},
		{"password", func(c *config.DatabaseConfig) { c.Password = "" }},
		{"name", func(c *config.DatabaseConfig) { c.Name = "" }},
		{"different_database", func(c *config.DatabaseConfig) { c.Name = "other" }},
		{"port_zero", func(c *config.DatabaseConfig) { c.Port = "0" }},
		{"port_overflow", func(c *config.DatabaseConfig) { c.Port = "65536" }},
		{"port_text", func(c *config.DatabaseConfig) { c.Port = cliTestSecret }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.change(&cfg)
			err := validateDatabaseConfig(cfg, opts)
			if err == nil {
				t.Fatal("invalid database configuration accepted")
			}
			if strings.Contains(err.Error(), cliTestSecret) {
				t.Fatal("database configuration leaked")
			}
		})
	}
}

func TestReindexMapping(t *testing.T) {
	valid := `{"settings":{},"mappings":{}}`

	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", valid, true},
		{"empty", "", false},
		{"null", "null", false},
		{"array", "[]", false},
		{"invalid_json", "{", false},
		{"missing_settings", `{"mappings":{}}`, false},
		{"null_settings", `{"settings":null,"mappings":{}}`, false},
		{"array_mappings", `{"settings":{},"mappings":[]}`, false},
		{"aliases", `{"settings":{},"mappings":{},"aliases":{}}`, false},
		{"trailing_json", valid + "{}", false},
		{"exact_limit",
			valid + strings.Repeat(" ", elasticsearch.MaxRequestBytes-len(valid)),
			true},
		{"over_limit",
			valid + strings.Repeat(" ", elasticsearch.MaxRequestBytes-len(valid)+1),
			false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := cliWriteMapping(t, tc.body)
			got, err := readDefinition(path)
			if tc.ok {
				if err != nil || string(got) != tc.body {
					t.Fatalf("valid mapping rejected or changed: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("invalid mapping returned successful or partial output")
			}
		})
	}

	t.Run("missing_or_directory", func(t *testing.T) {
		directory := t.TempDir()
		for _, path := range []string{
			directory,
			filepath.Join(directory, cliTestSecret),
		} {
			got, err := readDefinition(path)
			if err == nil || got != nil {
				t.Fatal("invalid mapping path accepted")
			}
			if strings.Contains(err.Error(), cliTestSecret) {
				t.Fatal("mapping path leaked")
			}
		}
	})
}

func TestReindexUniqueIndexName(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0,
		time.FixedZone("UTC+8", 8*60*60))
	pattern := regexp.MustCompile(
		`^rule_chunks_v1_20261029t040000z_[0-9a-f]{24}$`,
	)
	seen := make(map[string]bool)

	for i := 0; i < 64; i++ {
		name, err := generateIndexName(at)
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(name) {
			t.Fatalf("invalid index name: %s", name)
		}
		if seen[name] {
			t.Fatalf("duplicate generated index name: %s", name)
		}
		seen[name] = true
	}
}

func TestReindexRunPreflight(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		setup func(*testing.T) []string
	}{
		{"arguments", "arguments", func(t *testing.T) []string {
			return []string{"-timeout", cliTestSecret}
		}},
		{"es_configuration", "configuration", func(t *testing.T) []string {
			t.Setenv("ES_URL", "http://user:"+cliTestSecret+"@127.0.0.1:9200")
			return cliArguments()
		}},
		{"database_configuration", "configuration", func(t *testing.T) []string {
			t.Setenv("DB_NAME", "wrong_database")
			return cliArguments()
		}},
		{"mapping", "mapping", func(t *testing.T) []string {
			path := cliWriteMapping(t, "invalid JSON "+cliTestSecret)
			return append(cliArguments(), "-mapping", path)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cliEnvironment(t)
			args := tc.setup(t)
			var stdout, stderr bytes.Buffer

			code := run(args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("expected exit 2, got %d", code)
			}
			if strings.Contains(stdout.String()+stderr.String(), cliTestSecret) {
				t.Fatal("preflight output leaked a sensitive value")
			}

			decoder := json.NewDecoder(&stdout)
			var report finalReport
			if err := decoder.Decode(&report); err != nil {
				t.Fatal(err)
			}
			if report.Event != "finished" ||
				report.Stage != tc.stage ||
				report.ExitCode != 2 ||
				report.Result != nil ||
				report.NewIndex != "" ||
				len(report.ErrorClasses) == 0 {
				t.Fatalf("invalid preflight report: %+v", report)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				t.Fatal("unexpected extra output or started event")
			}
		})
	}

	t.Run("help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
			t.Fatalf("help returned %d", code)
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "-expect-db") {
			t.Fatal("help output is missing or emitted a rebuild report")
		}
	})
}

type cliBrokenWriter struct{}

func (cliBrokenWriter) Write([]byte) (int, error) {
	return 0, errors.New(cliTestSecret)
}

func TestReindexOutputFailure(t *testing.T) {
	t.Run("final_report", func(t *testing.T) {
		var stderr bytes.Buffer
		code := run(nil, cliBrokenWriter{}, &stderr)
		if code != 1 ||
			!strings.Contains(stderr.String(), "Cannot write final report") {
			t.Fatalf("final output failure not reported: %d, %s", code, &stderr)
		}
		if strings.Contains(stderr.String(), cliTestSecret) {
			t.Fatal("writer error leaked")
		}
	})

	t.Run("start_record", func(t *testing.T) {
		cliEnvironment(t)
		path := cliWriteMapping(t, `{"settings":{},"mappings":{}}`)
		args := append(cliArguments(), "-mapping", path)
		var stderr bytes.Buffer

		code := run(args, cliBrokenWriter{}, &stderr)
		if code != 1 ||
			!strings.Contains(stderr.String(), "rebuild was not started") {
			t.Fatalf("start output failure not reported: %d, %s", code, &stderr)
		}
		if strings.Contains(stderr.String(), cliTestSecret) {
			t.Fatal("writer error leaked")
		}
	})
}

func TestReindexCanceledExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	db, pool, err := openSourceDatabase(ctx, config.DatabaseConfig{})
	if db != nil || pool != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled connection returned resources or lost error: %v", err)
	}

	result, err := execute(
		ctx, config.Config{}, options{}, "test_new_index", nil,
	)
	if !errors.Is(err, context.Canceled) ||
		result == nil ||
		result.Stage != "connect" ||
		result.Publication.State != indexing.PublicationNotAttempted {
		t.Fatalf("canceled execution result incorrect: %+v, %v", result, err)
	}
}

func TestReindexErrorClassification(t *testing.T) {
	err := errors.Join(
		context.DeadlineExceeded,
		elasticsearch.ErrStateUnknown,
		elasticsearch.ErrUnavailable,
		errors.New(cliTestSecret),
	)

	classes := classifyError(err)
	for _, expected := range []string{
		"deadline_exceeded", "es_state_unknown", "es_unavailable",
	} {
		found := false
		for _, class := range classes {
			if class == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing error class %s in %v", expected, classes)
		}
	}
	if strings.Contains(strings.Join(classes, ","), cliTestSecret) {
		t.Fatal("raw error leaked into error classes")
	}
	if empty := classifyError(nil); empty == nil || len(empty) != 0 {
		t.Fatal("success must return a non-nil empty class list")
	}
	if got := classifyError(errors.New(cliTestSecret)); !reflect.DeepEqual(got, []string{"operation_failed"}) {
		t.Fatalf("unexpected fallback classification: %v", got)
	}
}
