package confirmation_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/service"
)

// 公开、固定、仅用于本文件的虚构测试配置；不得用于生产。
func signerFixture() (config.AppealConfirmationConfig, time.Time, service.AppealConfirmationBinding) {
	return config.AppealConfirmationConfig{
			KeyBase64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x53}, 32)),
			Issuer:    "test-only-issuer", Audience: "test-only-audience",
		}, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		service.AppealConfirmationBinding{
			UserID: 7, ConversationID: 9,
			IdempotencyKey: "00000000-0000-4000-8000-000000000001",
			RequestHash:    strings.Repeat("a", 64), PolicyDigest: strings.Repeat("b", 64),
		}
}

func mustSigner(t *testing.T, cfg config.AppealConfirmationConfig, clock func() time.Time) *confirmation.AppealSigner {
	t.Helper()
	s, err := confirmation.NewAppealSignerWithClock(cfg, clock)
	if err != nil || s == nil {
		t.Fatalf("construct signer: %v", err)
	}
	return s
}

func mustIssue(t *testing.T, s *confirmation.AppealSigner, b service.AppealConfirmationBinding, at time.Time) string {
	t.Helper()
	token, err := s.Issue(context.Background(), b, at, at.Add(10*time.Minute))
	if err != nil || token == "" {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

func wantSignerError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v; want %v", err, want)
	}
	if want == service.ErrAppealConfirmationRequired || want == service.ErrAppealConfiguration {
		if err.Error() != want.Error() {
			t.Fatal("public error contains unexpected detail")
		}
	}
}

// 按公开ac1格式构造隔离测试样本，不调用生产claims/mac辅助函数。
func fixturePayload(at time.Time) string {
	return fmt.Sprintf(
		`{"v":1,"purpose":"create_appeal_ticket","iss":"test-only-issuer","aud":"test-only-audience","user_id":"7","conversation_id":"9","key":"00000000-0000-4000-8000-000000000001","request_hash":"%s","policy_digest":"%s","iat_ms":%d,"exp_ms":%d}`,
		strings.Repeat("a", 64), strings.Repeat("b", 64),
		at.UnixMilli(), at.Add(10*time.Minute).UnixMilli(),
	)
}

func fixtureToken(t *testing.T, payload string) string {
	t.Helper()
	cfg, _, _ := signerFixture()
	key, err := base64.StdEncoding.DecodeString(cfg.KeyBase64)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := "ac1." + base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("video-support-agent/appeal-confirmation/ac1\n"))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestSignerContract(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	t.Run("independent_wire", func(t *testing.T) {
		token := mustIssue(t, s, binding, at)
		expected := fixtureToken(t, fixturePayload(at))
		if token != expected {
			t.Fatal("token differs from ac1 contract fixture")
		}
		if err := s.Verify(context.Background(), expected, binding); err != nil {
			t.Fatal(err)
		}
		if len(token) > 8192 {
			t.Fatal("token exceeds contract limit")
		}
	})
	t.Run("timezone_equivalence", func(t *testing.T) {
		local := at.In(time.FixedZone("test-offset", 8*60*60))
		if mustIssue(t, s, binding, local) != mustIssue(t, s, binding, at) {
			t.Fatal("same instant produced different token")
		}
	})
	t.Run("clock_millisecond_normalization", func(t *testing.T) {
		precise := mustSigner(t, cfg, func() time.Time { return at.Add(500 * time.Microsecond) })
		token := mustIssue(t, precise, binding, at)
		if err := precise.Verify(context.Background(), token, binding); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSignerConfiguration(t *testing.T) {
	cfg, at, _ := signerFixture()
	cases := []struct {
		name  string
		edit  func(*config.AppealConfirmationConfig)
		valid bool
	}{
		{"valid", func(c *config.AppealConfirmationConfig) {}, true},
		{"missing_key", func(c *config.AppealConfirmationConfig) { c.KeyBase64 = "" }, false},
		{"short_key", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = base64.StdEncoding.EncodeToString(make([]byte, 31))
		}, false},
		{"long_key", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = base64.StdEncoding.EncodeToString(make([]byte, 33))
		}, false},
		{"invalid_base64", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = strings.Repeat("!", 44)
		}, false},
		{"unpadded_key", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = strings.TrimRight(c.KeyBase64, "=")
		}, false},
		{"key_whitespace", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = " " + c.KeyBase64
		}, false},
		{"empty_issuer", func(c *config.AppealConfirmationConfig) { c.Issuer = "" }, false},
		{"issuer_space", func(c *config.AppealConfirmationConfig) { c.Issuer = "test issuer" }, false},
		{"issuer_unicode", func(c *config.AppealConfirmationConfig) { c.Issuer = "测试" }, false},
		{"issuer_too_long", func(c *config.AppealConfirmationConfig) {
			c.Issuer = strings.Repeat("i", 129)
		}, false},
		{"empty_audience", func(c *config.AppealConfirmationConfig) { c.Audience = "" }, false},
		{"audience_control", func(c *config.AppealConfirmationConfig) { c.Audience = "test\n" }, false},
		{"audience_too_long", func(c *config.AppealConfirmationConfig) {
			c.Audience = strings.Repeat("a", 129)
		}, false},
		{"scope_maximum", func(c *config.AppealConfirmationConfig) {
			c.Issuer = strings.Repeat("i", 128)
			c.Audience = strings.Repeat("a", 128)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := cfg
			tc.edit(&changed)
			got, err := confirmation.NewAppealSignerWithClock(changed, func() time.Time { return at })
			if tc.valid {
				if err != nil || got == nil {
					t.Fatalf("valid configuration rejected: %v", err)
				}
			} else {
				wantSignerError(t, err, service.ErrAppealConfiguration)
				if got != nil {
					t.Fatal("invalid configuration returned a signer")
				}
			}
		})
	}
	t.Run("nil_clock", func(t *testing.T) {
		got, err := confirmation.NewAppealSignerWithClock(cfg, nil)
		wantSignerError(t, err, service.ErrAppealConfiguration)
		if got != nil {
			t.Fatal("nil clock returned a signer")
		}
	})
}

func TestSignerIssueTime(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	cases := []struct {
		name            string
		issued, expires time.Time
		valid           bool
	}{
		{"at_now", at, at.Add(10 * time.Minute), true},
		{"still_valid", at.Add(-time.Minute), at.Add(9 * time.Minute), true},
		{"future", at.Add(time.Millisecond), at.Add(10*time.Minute + time.Millisecond), false},
		{"already_expired", at.Add(-10 * time.Minute), at, false},
		{"short_lifetime", at, at.Add(10*time.Minute - time.Millisecond), false},
		{"long_lifetime", at, at.Add(10*time.Minute + time.Millisecond), false},
		{"zero_issued", time.Time{}, at, false},
		{"zero_expires", at, time.Time{}, false},
		{"submillisecond", at.Add(time.Nanosecond), at.Add(10*time.Minute + time.Nanosecond), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := s.Issue(context.Background(), binding, tc.issued, tc.expires)
			if tc.valid {
				if err != nil || token == "" {
					t.Fatalf("valid issuance failed: %v", err)
				}
			} else {
				wantSignerError(t, err, service.ErrInvalidArgument)
				if token != "" {
					t.Fatal("rejected issuance returned a token")
				}
			}
		})
	}
}

func TestSignerVerifyTime(t *testing.T) {
	cfg, at, binding := signerFixture()
	now := at
	s := mustSigner(t, cfg, func() time.Time { return now })
	token := mustIssue(t, s, binding, at)
	cases := []struct {
		name  string
		delta time.Duration
		valid bool
	}{
		{"before_issue", -time.Millisecond, false},
		{"at_issue", 0, true},
		{"inside", 5 * time.Minute, true},
		{"last_millisecond", 10*time.Minute - time.Millisecond, true},
		{"at_expiry", 10 * time.Minute, false},
		{"after_expiry", 10*time.Minute + time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now = at.Add(tc.delta)
			err := s.Verify(context.Background(), token, binding)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantSignerError(t, err, service.ErrAppealConfirmationRequired)
			}
		})
	}
}

func TestSignerBinding(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	token := mustIssue(t, s, binding, at)
	cases := []struct {
		name string
		edit func(*service.AppealConfirmationBinding)
	}{
		{"user", func(b *service.AppealConfirmationBinding) { b.UserID++ }},
		{"conversation", func(b *service.AppealConfirmationBinding) { b.ConversationID++ }},
		{"operation_key", func(b *service.AppealConfirmationBinding) {
			b.IdempotencyKey = "00000000-0000-4000-8000-000000000002"
		}},
		{"request_hash", func(b *service.AppealConfirmationBinding) {
			b.RequestHash = strings.Repeat("c", 64)
		}},
		{"policy_digest", func(b *service.AppealConfirmationBinding) {
			b.PolicyDigest = strings.Repeat("c", 64)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := binding
			tc.edit(&other)
			wantSignerError(t, s.Verify(context.Background(), token, other), service.ErrAppealConfirmationRequired)
		})
	}
}

func TestSignerInvalidBinding(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	token := mustIssue(t, s, binding, at)
	cases := []struct {
		name string
		edit func(*service.AppealConfirmationBinding)
		want error
	}{
		{"zero_user", func(b *service.AppealConfirmationBinding) { b.UserID = 0 }, service.ErrUnauthorized},
		{"zero_conversation", func(b *service.AppealConfirmationBinding) {
			b.ConversationID = 0
		}, service.ErrInvalidArgument},
		{"invalid_key", func(b *service.AppealConfirmationBinding) {
			b.IdempotencyKey = "not-a-uuid"
		}, service.ErrInvalidArgument},
		{"invalid_hash", func(b *service.AppealConfirmationBinding) {
			b.RequestHash = "short"
		}, service.ErrInvalidArgument},
		{"invalid_digest", func(b *service.AppealConfirmationBinding) {
			b.PolicyDigest = "short"
		}, service.ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := binding
			tc.edit(&bad)
			got, err := s.Issue(context.Background(), bad, at, at.Add(10*time.Minute))
			wantSignerError(t, err, tc.want)
			if got != "" {
				t.Fatal("invalid binding returned a token")
			}
			wantSignerError(t, s.Verify(context.Background(), token, bad), tc.want)
		})
	}
}

func TestSignerScope(t *testing.T) {
	cfg, at, binding := signerFixture()
	token := mustIssue(t, mustSigner(t, cfg, func() time.Time { return at }), binding, at)
	cases := []struct {
		name string
		edit func(*config.AppealConfirmationConfig)
	}{
		{"different_key", func(c *config.AppealConfirmationConfig) {
			c.KeyBase64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x54}, 32))
		}},
		{"different_issuer", func(c *config.AppealConfirmationConfig) {
			c.Issuer = "another-test-issuer"
		}},
		{"different_audience", func(c *config.AppealConfirmationConfig) {
			c.Audience = "another-test-audience"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := cfg
			tc.edit(&other)
			verifier := mustSigner(t, other, func() time.Time { return at })
			wantSignerError(t, verifier.Verify(context.Background(), token, binding), service.ErrAppealConfirmationRequired)
		})
	}
}

func TestSignerEnvelope(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	token := mustIssue(t, s, binding, at)
	parts := strings.Split(token, ".")
	rawMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	rawMAC[0] ^= 1
	cases := []struct{ name, token string }{
		{"empty", ""},
		{"oversize", strings.Repeat("a", 8193)},
		{"wrong_prefix", "ac2." + parts[1] + "." + parts[2]},
		{"extra_segment", token + ".extra"},
		{"missing_segment", parts[0] + "." + parts[1]},
		{"payload_changed", "ac1." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + "." + parts[2]},
		{"mac_changed", parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(rawMAC)},
		{"mac_padding", token + "="},
		{"mac_invalid_base64", parts[0] + "." + parts[1] + ".!"},
		{"mac_wrong_size", parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 31))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantSignerError(t, s.Verify(context.Background(), tc.token, binding), service.ErrAppealConfirmationRequired)
		})
	}
}

func TestSignerClaims(t *testing.T) {
	cfg, at, binding := signerFixture()
	s := mustSigner(t, cfg, func() time.Time { return at })
	payload := fixturePayload(at)
	// 样本使用本文件的公开测试密钥签名，验证载荷规则而非只测MAC不匹配。
	cases := []struct{ name, payload string }{
		{"version", strings.Replace(payload, `"v":1`, `"v":2`, 1)},
		{"purpose", strings.Replace(payload, "create_appeal_ticket", "test_other_purpose", 1)},
		{"issuer", strings.Replace(payload, "test-only-issuer", "test-other-issuer", 1)},
		{"audience", strings.Replace(payload, "test-only-audience", "test-other-audience", 1)},
		{"numeric_id", strings.Replace(payload, `"user_id":"7"`, `"user_id":7`, 1)},
		{"duplicate_field", `{"v":1,` + payload[1:]},
		{"unknown_field", payload[:len(payload)-1] + `,"extra":true}`},
		{"null_field", strings.Replace(payload, `"iss":"test-only-issuer"`, `"iss":null`, 1)},
		{"missing_field", strings.Replace(payload, `"aud":"test-only-audience",`, "", 1)},
		{"whitespace", " " + payload},
		{"trailing_json", payload + "{}"},
		{"invalid_json", "{"},
		{"wrong_lifetime", strings.Replace(payload,
			fmt.Sprint(at.Add(10*time.Minute).UnixMilli()),
			fmt.Sprint(at.Add(11*time.Minute).UnixMilli()), 1)},
		{"future_issue", fixturePayload(at.Add(time.Millisecond))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantSignerError(t, s.Verify(context.Background(), fixtureToken(t, tc.payload), binding), service.ErrAppealConfirmationRequired)
		})
	}
}

func TestSignerContext(t *testing.T) {
	cfg, at, binding := signerFixture()
	valid := mustSigner(t, cfg, func() time.Time { return at })
	token := mustIssue(t, valid, binding, at)
	t.Run("cancel_before", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := valid.Issue(ctx, binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, context.Canceled)
		if got != "" {
			t.Fatal("canceled issuance returned a token")
		}
		wantSignerError(t, valid.Verify(ctx, token, binding), context.Canceled)
	})
	t.Run("deadline_before", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancel()
		got, err := valid.Issue(ctx, binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, context.DeadlineExceeded)
		if got != "" {
			t.Fatal("expired context returned a token")
		}
		wantSignerError(t, valid.Verify(ctx, token, binding), context.DeadlineExceeded)
	})
	t.Run("cancel_during_issue", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := mustSigner(t, cfg, func() time.Time { cancel(); return at })
		got, err := s.Issue(ctx, binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, context.Canceled)
		if got != "" {
			t.Fatal("late cancellation returned a token")
		}
	})
	t.Run("cancel_during_verify", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s := mustSigner(t, cfg, func() time.Time { cancel(); return at })
		wantSignerError(t, s.Verify(ctx, token, binding), context.Canceled)
	})
	t.Run("nil_signer", func(t *testing.T) {
		var s *confirmation.AppealSigner
		got, err := s.Issue(context.Background(), binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, service.ErrAppealConfiguration)
		if got != "" {
			t.Fatal("nil signer returned a token")
		}
		wantSignerError(t, s.Verify(context.Background(), token, binding), service.ErrAppealConfiguration)
	})
	t.Run("zero_signer", func(t *testing.T) {
		s := &confirmation.AppealSigner{}
		got, err := s.Issue(context.Background(), binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, service.ErrAppealConfiguration)
		if got != "" {
			t.Fatal("zero signer returned a token")
		}
		wantSignerError(t, s.Verify(context.Background(), token, binding), service.ErrAppealConfiguration)
	})
	t.Run("invalid_clock", func(t *testing.T) {
		s := mustSigner(t, cfg, func() time.Time { return time.Time{} })
		got, err := s.Issue(context.Background(), binding, at, at.Add(10*time.Minute))
		wantSignerError(t, err, service.ErrAppealConfiguration)
		if got != "" {
			t.Fatal("invalid clock returned a token")
		}
		wantSignerError(t, s.Verify(context.Background(), token, binding), service.ErrAppealConfiguration)
	})
}

func TestSignerConfigLoad(t *testing.T) {
	cfg, _, _ := signerFixture()
	t.Setenv("JWT_SECRET", "test-only-login-secret")
	t.Run("defaults_no_key", func(t *testing.T) {
		t.Setenv("APPEAL_CONFIRMATION_KEY_BASE64", "")
		t.Setenv("APPEAL_CONFIRMATION_ISSUER", "")
		t.Setenv("APPEAL_CONFIRMATION_AUDIENCE", "")
		got := config.Load().AppealConfirmation
		if got.KeyBase64 != "" || got.Issuer != "video-support-agent-appeal" || got.Audience != "video-support-agent-appeal-create" {
			t.Fatal("unexpected defaults or login secret reused")
		}
		_, err := confirmation.NewAppealSigner(got)
		wantSignerError(t, err, service.ErrAppealConfiguration)
	})
	t.Run("explicit_environment", func(t *testing.T) {
		t.Setenv("APPEAL_CONFIRMATION_KEY_BASE64", cfg.KeyBase64)
		t.Setenv("APPEAL_CONFIRMATION_ISSUER", cfg.Issuer)
		t.Setenv("APPEAL_CONFIRMATION_AUDIENCE", cfg.Audience)
		if config.Load().AppealConfirmation != cfg {
			t.Fatal("confirmation config was not loaded exactly")
		}
	})
}
