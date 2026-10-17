package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSafeLoggingDoesNotExposeRequestSecrets(t *testing.T) {
	core, observed := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	r := gin.New()
	r.Use(SafeAccessLog(logger), SafeRecovery(logger))
	r.POST("/test", func(c *gin.Context) {
		panic("secret-panic-marker")
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/test?token=secret-query-marker",
		strings.NewReader(`{"password":"secret-body-marker"}`),
	)
	req.Header.Set("Authorization", "Bearer secret-header-marker")
	req.Header.Set("Cookie", "session=secret-cookie-marker")

	recorder := httptest.NewRecorder()
	r.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}

	if observed.Len() != 2 {
		t.Fatalf("expected panic and access logs, got %d", observed.Len())
	}

	for _, entry := range observed.All() {
		encoded, err := zapcore.NewJSONEncoder(
			zap.NewProductionEncoderConfig(),
		).EncodeEntry(entry.Entry, entry.Context)
		if err != nil {
			t.Fatal(err)
		}

		logText := encoded.String()
		encoded.Free()

		if strings.Contains(logText, "secret-") {
			t.Fatalf("sensitive marker found in logs")
		}
	}

	if strings.Contains(recorder.Body.String(), "secret-") {
		t.Fatal("sensitive marker found in response")
	}
}
