package main

import (
	"strings"
	"testing"
)

func TestRunRejectsInvalidElasticsearchConfig(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{"invalid_url", "ES_URL", "ftp://localhost:9200"},
		{"invalid_alias", "ES_INDEX_ALIAS", "Rules"},
		{"invalid_timeout", "ES_TIMEOUT", "0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ES_URL", "http://127.0.0.1:9200")
			t.Setenv("ES_INDEX_ALIAS", "rule_chunks_current")
			t.Setenv("ES_TIMEOUT", "5s")
			t.Setenv(tt.field, tt.value)

			// 后续代理校验应当拒绝此值，形成外部连接之前的保护。
			// 正确路径必须先返回ES配置错误，而不是走到代理校验。
			t.Setenv("TRUSTED_PROXIES", "not-an-ip-or-cidr")

			err := run()
			if err == nil {
				t.Fatal("run accepted invalid Elasticsearch configuration")
			}
			if !strings.HasPrefix(err.Error(), tt.field+" ") {
				t.Fatalf(
					"expected %s error before later initialization, got %q",
					tt.field,
					err.Error(),
				)
			}
		})
	}
}
