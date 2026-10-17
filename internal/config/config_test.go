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
