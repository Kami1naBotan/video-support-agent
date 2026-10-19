package config

import "testing"

func TestLoadJWTDefaults(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	t.Setenv("JWT_ISSUER", "")
	t.Setenv("JWT_TTL", "")

	cfg := Load()

	if cfg.JWT.Secret != "" {
		t.Fatal("JWT secret must not have a default value")
	}
	if cfg.JWT.Issuer != "bili-support" {
		t.Fatalf("unexpected issuer: %q", cfg.JWT.Issuer)
	}
	if cfg.JWT.TTL != "2h" {
		t.Fatalf("unexpected TTL: %q", cfg.JWT.TTL)
	}
}

func TestLoadJWTEnvironment(t *testing.T) {
	// 仅用于测试配置读取，不用于实际签名。
	secret := "test-only-secret-from-environment"

	t.Setenv("JWT_SECRET", secret)
	t.Setenv("JWT_ISSUER", "test-support")
	t.Setenv("JWT_TTL", "30m")

	cfg := Load()

	if cfg.JWT.Secret != secret {
		t.Fatal("JWT secret was not loaded from environment")
	}
	if cfg.JWT.Issuer != "test-support" {
		t.Fatalf("unexpected issuer: %q", cfg.JWT.Issuer)
	}
	if cfg.JWT.TTL != "30m" {
		t.Fatalf("unexpected TTL: %q", cfg.JWT.TTL)
	}
}

func TestLoadLLMDefaults(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_TIMEOUT", "")

	cfg := Load()

	if cfg.LLM.BaseURL != "" {
		t.Fatal("LLM base URL must not have a default value")
	}
	if cfg.LLM.Model != "" {
		t.Fatal("LLM model must not have a default value")
	}
	if cfg.LLM.APIKey != "" {
		t.Fatal("LLM API key must not have a default value")
	}
	if cfg.LLM.Timeout != "30s" {
		t.Fatalf("unexpected LLM timeout: %q", cfg.LLM.Timeout)
	}
}

func TestLoadLLMEnvironment(t *testing.T) {
	// 仅用于测试配置读取，不是真实密钥，不会发送网络请求。
	const testKey = "test-only-api-key"

	t.Setenv("LLM_BASE_URL", "https://example.com/v1")
	t.Setenv("LLM_MODEL", "test-model")
	t.Setenv("LLM_API_KEY", testKey)
	t.Setenv("LLM_TIMEOUT", "45s")

	cfg := Load()

	if cfg.LLM.BaseURL != "https://example.com/v1" {
		t.Fatalf("unexpected LLM base URL: %q", cfg.LLM.BaseURL)
	}
	if cfg.LLM.Model != "test-model" {
		t.Fatalf("unexpected LLM model: %q", cfg.LLM.Model)
	}
	if cfg.LLM.APIKey != testKey {
		t.Fatal("LLM API key was not loaded from environment")
	}
	if cfg.LLM.Timeout != "45s" {
		t.Fatalf("unexpected LLM timeout: %q", cfg.LLM.Timeout)
	}
}
