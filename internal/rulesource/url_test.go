package rulesource_test

import (
	"strings"
	"testing"

	"video-support-agent/internal/rulesource"
)

func TestRAGOfficialSourcePolicy(t *testing.T) {
	valid := []string{
		"https://www.bilibili.com/",
		"http://www.bilibili.com/",
		"https://www.bilibili.com:443/test",
		"http://www.bilibili.com:80/test",
		"https://WWW.BILIBILI.COM/test",
		"  https://www.bilibili.com/test  ",
	}
	for _, raw := range valid {
		t.Run("accept_"+raw, func(t *testing.T) {
			if err := rulesource.ValidateOfficialURL(raw); err != nil {
				t.Fatalf("valid source rejected: %v", err)
			}
		})
	}

	invalid := []struct {
		name string
		raw  string
	}{
		{"blank", " "},
		{"relative", "/test"},
		{"other_scheme", "ftp://www.bilibili.com/test"},
		{"other_host", "https://example.org/test"},
		{"credentials", "https://user:password@www.bilibili.com/test"},
		{"nonstandard_port", "https://www.bilibili.com:8443/test"},
		{"empty_port", "https://www.bilibili.com:/test"},
		{"too_long", "https://www.bilibili.com/" + strings.Repeat("a", 2048)},
	}
	for _, tc := range invalid {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			if err := rulesource.ValidateOfficialURL(tc.raw); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}

	t.Run("2048_character_boundary", func(t *testing.T) {
		prefix := "https://www.bilibili.com/"
		exact := prefix + strings.Repeat("a", 2048-len(prefix))
		if err := rulesource.ValidateOfficialURL(exact); err != nil {
			t.Fatalf("exact boundary rejected: %v", err)
		}
		if err := rulesource.ValidateOfficialURL(exact + "a"); err == nil {
			t.Fatal("over-limit source accepted")
		}
	})
}
