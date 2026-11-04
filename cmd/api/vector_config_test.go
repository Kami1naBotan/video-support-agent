package main

import (
	"strings"
	"testing"
)

func TestRunRejectsInvalidVectorConfig(t *testing.T) {
	// 仅用于配置校验测试，不用于运行服务。
	t.Setenv("APPEAL_CONFIRMATION_KEY_BASE64",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("APPEAL_CONFIRMATION_ISSUER", "config-test")
	t.Setenv("APPEAL_CONFIRMATION_AUDIENCE", "config-test")

	cases := []struct {
		field string
		value string
	}{
		{"EMBEDDING_URL", "http://user:SECRET_SENTINEL@localhost:11434"},
		{"EMBEDDING_MODEL", "unsupported"},
		{"EMBEDDING_MODEL_DIGEST", "SECRET_SENTINEL"},
		{"EMBEDDING_DIMENSIONS", "768"},
		{"EMBEDDING_CONTEXT_LENGTH", "4096"},
		{"EMBEDDING_TIMEOUT", "0s"},
		{"MILVUS_URL", "ftp://localhost:19530"},
		{"MILVUS_DATABASE", "1invalid"},
		{"MILVUS_COLLECTION_ALIAS", "invalid-alias"},
		{"MILVUS_TIMEOUT", "-1s"},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			baseline := map[string]string{
				"ES_URL":                   "http://127.0.0.1:9200",
				"ES_INDEX_ALIAS":           "rule_chunks_current",
				"ES_TIMEOUT":               "5s",
				"EMBEDDING_URL":            "http://127.0.0.1:11434",
				"EMBEDDING_MODEL":          "bge-m3:567m",
				"EMBEDDING_MODEL_DIGEST":   strings.Repeat("a", 64),
				"EMBEDDING_DIMENSIONS":     "1024",
				"EMBEDDING_CONTEXT_LENGTH": "8192",
				"EMBEDDING_TIMEOUT":        "60s",
				"MILVUS_URL":               "http://127.0.0.1:19530",
				"MILVUS_DATABASE":          "default",
				"MILVUS_COLLECTION_ALIAS":  "rule_chunks_vector_current",
				"MILVUS_TIMEOUT":           "5s",
				"TRUSTED_PROXIES":          "not-an-ip-or-cidr",
			}
			for key, value := range baseline {
				t.Setenv(key, value)
			}
			t.Setenv(tc.field, tc.value)

			// 无效代理配置是后续初始化之前的保护。
			// 必须先得到向量配置错误，不能走到代理校验或数据库初始化。
			err := run()
			if err == nil {
				t.Fatal("run accepted invalid vector configuration")
			}
			if !strings.HasPrefix(err.Error(), tc.field+" ") {
				t.Fatalf("expected %s error before later initialization, got %v",
					tc.field, err)
			}
			if strings.Contains(err.Error(), "SECRET_SENTINEL") {
				t.Fatal("startup error disclosed sensitive input")
			}
		})
	}
}
