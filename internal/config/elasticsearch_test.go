package config

import (
	"strings"
	"testing"
)

func TestLoadElasticsearchConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		alias   string
		timeout string
		want    ElasticsearchConfig
	}{
		{
			name: "empty_values_use_defaults",
			want: ElasticsearchConfig{
				URL:        "http://127.0.0.1:9200",
				IndexAlias: "rule_chunks_current",
				Timeout:    "5s",
			},
		},
		{
			name:    "environment_overrides_defaults",
			url:     "https://search.example.invalid:9243",
			alias:   "rules_test",
			timeout: "750ms",
			want: ElasticsearchConfig{
				URL:        "https://search.example.invalid:9243",
				IndexAlias: "rules_test",
				Timeout:    "750ms",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ES_URL", tt.url)
			t.Setenv("ES_INDEX_ALIAS", tt.alias)
			t.Setenv("ES_TIMEOUT", tt.timeout)

			got := Load().Elasticsearch
			if got != tt.want {
				t.Fatalf("configuration mismatch: got %+v, want %+v", got, tt.want)
			}
			if err := ValidateElasticsearchConfig(got); err != nil {
				t.Fatalf("loaded configuration rejected: %v", err)
			}
		})
	}
}

func TestValidateElasticsearchConfigAcceptsSupportedValues(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		alias   string
		timeout string
	}{
		{
			"local",
			"http://127.0.0.1:9200",
			"rule_chunks_current",
			"5s",
		},
		{
			"https_without_explicit_port",
			"https://search.example.invalid",
			"rules",
			"500ms",
		},
		{
			"trailing_slash",
			"http://localhost:9200/",
			"rules-v1",
			"1s",
		},
		{
			"ipv6",
			"http://[::1]:9200",
			"rules_1",
			"2s",
		},
		{
			"lowest_port_and_shortest_alias",
			"http://localhost:1",
			"a",
			"1ns",
		},
		{
			"highest_port_and_longest_alias",
			"http://localhost:65535",
			strings.Repeat("a", 255),
			"1m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ElasticsearchConfig{
				URL:        tt.url,
				IndexAlias: tt.alias,
				Timeout:    tt.timeout,
			}
			if err := ValidateElasticsearchConfig(cfg); err != nil {
				t.Fatalf("supported configuration rejected: %v", err)
			}
		})
	}
}

func TestValidateElasticsearchConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{"empty_url", "ES_URL", ""},
		{"leading_space", "ES_URL", " http://localhost:9200"},
		{"trailing_space", "ES_URL", "http://localhost:9200 "},
		{"missing_scheme", "ES_URL", "localhost:9200"},
		{"unsupported_scheme", "ES_URL", "ftp://localhost:9200"},
		{"missing_host", "ES_URL", "http:///"},
		{"opaque_url", "ES_URL", "http:localhost"},
		{"malformed_ipv6", "ES_URL", "http://[::1"},
		{"credentials", "ES_URL", "http://user:secret@localhost:9200"},
		{"query", "ES_URL", "http://localhost:9200?pretty=true"},
		{"empty_query", "ES_URL", "http://localhost:9200?"},
		{"fragment", "ES_URL", "http://localhost:9200#section"},
		{"empty_fragment", "ES_URL", "http://localhost:9200#"},
		{"path_prefix", "ES_URL", "http://localhost:9200/es"},
		{"encoded_path", "ES_URL", "http://localhost:9200/%2F"},
		{"empty_port", "ES_URL", "http://localhost:"},
		{"zero_port", "ES_URL", "http://localhost:0"},
		{"port_too_large", "ES_URL", "http://localhost:65536"},
		{"non_numeric_port", "ES_URL", "http://localhost:abc"},
		{"empty_alias", "ES_INDEX_ALIAS", ""},
		{"uppercase_alias", "ES_INDEX_ALIAS", "Rules"},
		{"alias_starts_underscore", "ES_INDEX_ALIAS", "_rules"},
		{"alias_starts_hyphen", "ES_INDEX_ALIAS", "-rules"},
		{"alias_contains_space", "ES_INDEX_ALIAS", "rule chunks"},
		{"alias_contains_slash", "ES_INDEX_ALIAS", "rules/current"},
		{"alias_contains_wildcard", "ES_INDEX_ALIAS", "rules*"},
		{"alias_too_long", "ES_INDEX_ALIAS", strings.Repeat("a", 256)},
		{"empty_timeout", "ES_TIMEOUT", ""},
		{"zero_timeout", "ES_TIMEOUT", "0s"},
		{"negative_timeout", "ES_TIMEOUT", "-1s"},
		{"timeout_without_unit", "ES_TIMEOUT", "5"},
		{"invalid_timeout", "ES_TIMEOUT", "invalid"},
		{"timeout_overflow", "ES_TIMEOUT", "999999999999999999999h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ElasticsearchConfig{
				URL:        "http://127.0.0.1:9200",
				IndexAlias: "rule_chunks_current",
				Timeout:    "5s",
			}

			switch tt.field {
			case "ES_URL":
				cfg.URL = tt.value
			case "ES_INDEX_ALIAS":
				cfg.IndexAlias = tt.value
			case "ES_TIMEOUT":
				cfg.Timeout = tt.value
			default:
				t.Fatalf("unknown test field: %s", tt.field)
			}

			err := ValidateElasticsearchConfig(cfg)
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if !strings.HasPrefix(err.Error(), tt.field+" ") {
				t.Fatalf("error must identify %s, got %q", tt.field, err.Error())
			}
		})
	}
}

func TestValidateElasticsearchConfigDoesNotExposeInput(t *testing.T) {
	const marker = "test-only-sensitive-marker"

	tests := []struct {
		name string
		cfg  ElasticsearchConfig
	}{
		{
			"credentials",
			ElasticsearchConfig{
				URL:        "http://user:" + marker + "@localhost:9200",
				IndexAlias: "rules",
				Timeout:    "5s",
			},
		},
		{
			"malformed_url",
			ElasticsearchConfig{
				URL:        "http://user:" + marker + "@[",
				IndexAlias: "rules",
				Timeout:    "5s",
			},
		},
		{
			"invalid_alias",
			ElasticsearchConfig{
				URL:        "http://localhost:9200",
				IndexAlias: marker + "/invalid",
				Timeout:    "5s",
			},
		},
		{
			"invalid_timeout",
			ElasticsearchConfig{
				URL:        "http://localhost:9200",
				IndexAlias: "rules",
				Timeout:    marker,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateElasticsearchConfig(tt.cfg)
			if err == nil {
				t.Fatal("invalid configuration was accepted")
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("error exposed the input marker")
			}
		})
	}
}
