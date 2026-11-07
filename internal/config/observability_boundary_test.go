package config

import (
	"os"
	"strings"
	"testing"
)

func TestMetricsListenerPermissionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, addr, permission string
		valid                  bool
	}{
		{"loopback_default", "127.0.0.1:9464", "", true},
		{"ipv6_loopback", "[::1]:9464", "false", true},
		{"wildcard_denied", "0.0.0.0:9464", "false", false},
		{"wildcard_allowed", "0.0.0.0:9464", "true", true},
		{"ipv6_wildcard_allowed", "[::]:9464", "true", true},
		{"private_denied", "10.0.0.1:9464", "", false},
		{"private_allowed", "10.0.0.1:9464", "true", true},
		{"invalid_permission", "127.0.0.1:9464", "yes", false},
		{"hostname_denied", "localhost:9464", "true", false},
		{"empty_host_denied", ":9464", "true", false},
		{"zero_port_denied", "0.0.0.0:0", "true", false},
		{"overflow_port_denied", "0.0.0.0:65536", "true", false},
		{"invalid_ip_denied", "999.1.1.1:9464", "true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ObservabilityConfig{Enabled: "true", ServiceName: "boundary", OTLPEndpoint: "http://collector.test:4318", SampleRatio: "1", MetricsAddr: tc.addr, MetricsAllowNonLoopback: tc.permission}
			settings, err := ParseObservabilityConfig(cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if err == nil && settings.MetricsAddr != tc.addr {
				t.Fatal("listener address changed")
			}
			if err != nil && strings.Contains(err.Error(), tc.addr) {
				t.Fatal("error exposed address")
			}
		})
	}
}

func TestObservabilityEndpointEnvironmentBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, enabled, endpoint string
		unset, valid            bool
	}{
		{"unset_keeps_local_default", "true", "", true, true},
		{"explicit_empty_enabled_rejected", "true", "", false, false},
		{"explicit_empty_disabled_allowed", "false", "", false, true},
		{"explicit_endpoint_enabled", "true", "http://collector.test:4318", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OBS_ENABLED", tc.enabled)
			t.Setenv("OBS_SERVICE_NAME", "boundary")
			t.Setenv("OBS_SAMPLE_RATIO", "1")
			t.Setenv("OBS_METRICS_ADDR", "127.0.0.1:9464")
			t.Setenv("OBS_METRICS_ALLOW_NON_LOOPBACK", "false")
			t.Setenv("OBS_OTLP_ENDPOINT", tc.endpoint)
			if tc.unset {
				if err := os.Unsetenv("OBS_OTLP_ENDPOINT"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := LoadObservabilityConfig()
			_, err := ParseObservabilityConfig(cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v; explicit empty endpoint must not become a default", tc.valid, err)
			}
			if tc.unset && cfg.OTLPEndpoint != "http://127.0.0.1:4318" {
				t.Fatal("local default changed")
			}
		})
	}
}
