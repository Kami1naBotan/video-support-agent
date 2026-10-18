package config

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidateTrustedProxies(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		wantErr bool
	}{
		{name: "empty"},
		{name: "single IP", input: []string{"192.0.2.10"}},
		{name: "CIDR", input: []string{"192.0.2.0/24"}},
		{name: "trim spaces", input: []string{" 192.0.2.10 "}},
		{name: "invalid", input: []string{"invalid"}, wantErr: true},
		{name: "empty entry", input: []string{""}, wantErr: true},
		{name: "all IPv4", input: []string{"0.0.0.0/0"}, wantErr: true},
		{name: "all IPv6", input: []string{"::/0"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateTrustedProxies(tt.input)

			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
		})
	}
}

func TestTrustedProxyClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		trusted []string
		remote  string
		wantIP  string
	}{
		{
			name:   "default ignores forwarded header",
			remote: "192.0.2.10:1234",
			wantIP: "192.0.2.10",
		},
		{
			name:    "trusted proxy uses forwarded header",
			trusted: []string{"192.0.2.10"},
			remote:  "192.0.2.10:1234",
			wantIP:  "198.51.100.8",
		},
		{
			name:    "other source cannot spoof IP",
			trusted: []string{"192.0.2.10"},
			remote:  "192.0.2.20:1234",
			wantIP:  "192.0.2.20",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxies, err := ValidateTrustedProxies(tt.trusted)
			if err != nil {
				t.Fatal(err)
			}

			r := gin.New()
			if err := r.SetTrustedProxies(proxies); err != nil {
				t.Fatal(err)
			}

			r.GET("/", func(c *gin.Context) {
				c.String(http.StatusOK, c.ClientIP())
			})

			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = tt.remote
			request.Header.Set("X-Forwarded-For", "198.51.100.8")

			recorder := httptest.NewRecorder()
			r.ServeHTTP(recorder, request)

			if recorder.Body.String() != tt.wantIP {
				t.Fatalf(
					"expected %q, got %q",
					tt.wantIP,
					recorder.Body.String(),
				)
			}
		})
	}
}
