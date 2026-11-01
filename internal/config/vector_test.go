package config

import (
	"strings"
	"testing"
)

func vectorConfigBaseline(t *testing.T) (EmbeddingConfig, MilvusConfig) {
	t.Helper()

	for _, key := range []string{
		"EMBEDDING_URL",
		"EMBEDDING_MODEL",
		"EMBEDDING_MODEL_DIGEST",
		"EMBEDDING_DIMENSIONS",
		"EMBEDDING_CONTEXT_LENGTH",
		"EMBEDDING_TIMEOUT",
		"MILVUS_URL",
		"MILVUS_DATABASE",
		"MILVUS_COLLECTION_ALIAS",
		"MILVUS_TIMEOUT",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()
	return cfg.Embedding, cfg.Milvus
}

func TestVectorLoadDefaultsAndOverrides(t *testing.T) {
	embedding, milvus := vectorConfigBaseline(t)

	wantEmbedding := EmbeddingConfig{
		URL:           "http://127.0.0.1:11434",
		Model:         "bge-m3:567m",
		ModelDigest:   "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab",
		Dimensions:    "1024",
		ContextLength: "8192",
		Timeout:       "60s",
	}
	wantMilvus := MilvusConfig{
		URL:             "http://127.0.0.1:19530",
		Database:        "default",
		CollectionAlias: "rule_chunks_vector_current",
		Timeout:         "5s",
	}

	if embedding != wantEmbedding || milvus != wantMilvus {
		t.Fatal("default vector configuration mismatch")
	}
	if err := ValidateEmbeddingConfig(embedding); err != nil {
		t.Fatalf("default embedding configuration rejected: %v", err)
	}
	if err := ValidateMilvusConfig(milvus); err != nil {
		t.Fatalf("default Milvus configuration rejected: %v", err)
	}

	values := map[string]string{
		"EMBEDDING_URL":            "https://embedding.example.invalid:11435/",
		"EMBEDDING_MODEL":          "other-model",
		"EMBEDDING_MODEL_DIGEST":   strings.Repeat("a", 64),
		"EMBEDDING_DIMENSIONS":     "invalid-dimension",
		"EMBEDDING_CONTEXT_LENGTH": "4096",
		"EMBEDDING_TIMEOUT":        "750ms",
		"MILVUS_URL":               "https://milvus.example.invalid:19531/",
		"MILVUS_DATABASE":          "rules_test",
		"MILVUS_COLLECTION_ALIAS":  "rules_vector_test",
		"MILVUS_TIMEOUT":           "250ms",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}

	cfg := Load()
	wantEmbedding = EmbeddingConfig{
		URL:           values["EMBEDDING_URL"],
		Model:         values["EMBEDDING_MODEL"],
		ModelDigest:   values["EMBEDDING_MODEL_DIGEST"],
		Dimensions:    values["EMBEDDING_DIMENSIONS"],
		ContextLength: values["EMBEDDING_CONTEXT_LENGTH"],
		Timeout:       values["EMBEDDING_TIMEOUT"],
	}
	wantMilvus = MilvusConfig{
		URL:             values["MILVUS_URL"],
		Database:        values["MILVUS_DATABASE"],
		CollectionAlias: values["MILVUS_COLLECTION_ALIAS"],
		Timeout:         values["MILVUS_TIMEOUT"],
	}
	if cfg.Embedding != wantEmbedding || cfg.Milvus != wantMilvus {
		t.Fatal("environment overrides were not preserved exactly")
	}

	// Load 不应把错误原值偷偷换回默认值。
	if err := ValidateEmbeddingConfig(cfg.Embedding); err == nil {
		t.Fatal("unsupported model configuration was accepted")
	}
	if err := ValidateMilvusConfig(cfg.Milvus); err != nil {
		t.Fatalf("valid overridden Milvus configuration rejected: %v", err)
	}
}

func TestVectorURLValidation(t *testing.T) {
	baseEmbedding, baseMilvus := vectorConfigBaseline(t)

	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"http", "http://127.0.0.1:11434", true},
		{"https_root", "https://example.invalid/", true},
		{"ipv6", "http://[::1]:19530", true},
		{"lowest_port", "http://example.invalid:1", true},
		{"highest_port", "http://example.invalid:65535", true},
		{"empty", "", false},
		{"leading_space", " http://localhost:19530", false},
		{"trailing_space", "http://localhost:19530 ", false},
		{"missing_scheme", "localhost:19530", false},
		{"ftp", "ftp://localhost:19530", false},
		{"missing_host", "http:///path", false},
		{"opaque", "http:localhost", false},
		{"credentials", "http://user:SECRET_SENTINEL@localhost:19530", false},
		{"query", "http://localhost:19530?token=SECRET_SENTINEL", false},
		{"empty_query", "http://localhost:19530?", false},
		{"fragment", "http://localhost:19530#SECRET_SENTINEL", false},
		{"empty_fragment", "http://localhost:19530#", false},
		{"api_path", "http://localhost:19530/api/embed", false},
		{"encoded_path", "http://localhost:19530/%2F", false},
		{"bad_escape", "http://localhost:19530/%zz", false},
		{"zero_port", "http://localhost:0", false},
		{"large_port", "http://localhost:65536", false},
		{"text_port", "http://localhost:abc", false},
		{"empty_port", "http://localhost:", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, m := baseEmbedding, baseMilvus
			e.URL, m.URL = tc.url, tc.url

			results := []struct {
				field string
				err   error
			}{
				{"EMBEDDING_URL", ValidateEmbeddingConfig(e)},
				{"MILVUS_URL", ValidateMilvusConfig(m)},
			}
			for _, result := range results {
				if tc.ok {
					if result.err != nil {
						t.Fatalf("%s rejected valid URL: %v", result.field, result.err)
					}
					continue
				}
				if result.err == nil {
					t.Fatalf("%s accepted invalid URL", result.field)
				}
				if !strings.HasPrefix(result.err.Error(), result.field+" ") {
					t.Fatalf("wrong error field: %v", result.err)
				}
				if strings.Contains(result.err.Error(), "SECRET_SENTINEL") {
					t.Fatal("error disclosed sensitive input")
				}
			}
		})
	}
}

func TestVectorEmbeddingContract(t *testing.T) {
	base, _ := vectorConfigBaseline(t)

	cases := []struct {
		name  string
		field string
		value string
		ok    bool
	}{
		{"model", "MODEL", "bge-m3:567m", true},
		{"other_model", "MODEL", "bge-m3:latest", false},
		{"empty_model", "MODEL", "", false},
		{"spaced_model", "MODEL", " bge-m3:567m", false},
		{"digest", "MODEL_DIGEST", strings.Repeat("a", 64), true},
		{"short_digest", "MODEL_DIGEST", strings.Repeat("a", 63), false},
		{"long_digest", "MODEL_DIGEST", strings.Repeat("a", 65), false},
		{"uppercase_digest", "MODEL_DIGEST", strings.Repeat("A", 64), false},
		{"invalid_digest", "MODEL_DIGEST", strings.Repeat("g", 64), false},
		{"dimensions", "DIMENSIONS", "1024", true},
		{"wrong_dimensions", "DIMENSIONS", "768", false},
		{"padded_dimensions", "DIMENSIONS", "01024", false},
		{"context", "CONTEXT_LENGTH", "8192", true},
		{"wrong_context", "CONTEXT_LENGTH", "4096", false},
		{"spaced_context", "CONTEXT_LENGTH", "8192 ", false},
		{"timeout", "TIMEOUT", "750ms", true},
		{"tiny_timeout", "TIMEOUT", "1ns", true},
		{"zero_timeout", "TIMEOUT", "0s", false},
		{"negative_timeout", "TIMEOUT", "-1s", false},
		{"missing_unit", "TIMEOUT", "60", false},
		{"overflow", "TIMEOUT", "999999999999999999999h", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			switch tc.field {
			case "MODEL":
				cfg.Model = tc.value
			case "MODEL_DIGEST":
				cfg.ModelDigest = tc.value
			case "DIMENSIONS":
				cfg.Dimensions = tc.value
			case "CONTEXT_LENGTH":
				cfg.ContextLength = tc.value
			case "TIMEOUT":
				cfg.Timeout = tc.value
			}

			err := ValidateEmbeddingConfig(cfg)
			if tc.ok {
				if err != nil {
					t.Fatalf("valid configuration rejected: %v", err)
				}
			} else if err == nil ||
				!strings.HasPrefix(err.Error(), "EMBEDDING_"+tc.field+" ") {
				t.Fatalf("expected field-specific rejection, got %v", err)
			}
		})
	}
}

func TestVectorMilvusNamesAndTimeout(t *testing.T) {
	_, base := vectorConfigBaseline(t)

	names := []struct {
		name  string
		value string
		ok    bool
	}{
		{"simple", "rules_01", true},
		{"underscore", "_rules", true},
		{"uppercase", "Rules", true},
		{"max_length", strings.Repeat("a", 255), true},
		{"too_long", strings.Repeat("a", 256), false},
		{"empty", "", false},
		{"digit_first", "1rules", false},
		{"hyphen", "rules-current", false},
		{"space", "rules current", false},
		{"unicode", "规则", false},
	}

	for _, field := range []string{"DATABASE", "COLLECTION_ALIAS"} {
		for _, tc := range names {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				cfg := base
				if field == "DATABASE" {
					cfg.Database = tc.value
				} else {
					cfg.CollectionAlias = tc.value
				}
				err := ValidateMilvusConfig(cfg)
				if tc.ok {
					if err != nil {
						t.Fatalf("valid identifier rejected: %v", err)
					}
				} else if err == nil ||
					!strings.HasPrefix(err.Error(), "MILVUS_"+field+" ") {
					t.Fatalf("expected identifier rejection, got %v", err)
				}
			})
		}
	}

	for _, value := range []string{"", "0", "0s", "-1s", "5", "bad"} {
		t.Run("timeout_reject/"+value, func(t *testing.T) {
			cfg := base
			cfg.Timeout = value
			err := ValidateMilvusConfig(cfg)
			if err == nil || !strings.HasPrefix(err.Error(), "MILVUS_TIMEOUT ") {
				t.Fatalf("expected timeout rejection, got %v", err)
			}
		})
	}

	cfg := base
	cfg.Timeout = "250ms"
	if err := ValidateMilvusConfig(cfg); err != nil {
		t.Fatalf("valid timeout rejected: %v", err)
	}
}
