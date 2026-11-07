package config

import (
	"errors"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
)

type ObservabilityConfig struct {
	Enabled                 string
	ServiceName             string
	OTLPEndpoint            string
	SampleRatio             string
	MetricsAddr             string
	MetricsAllowNonLoopback string
}

type ObservabilitySettings struct {
	Enabled      bool
	ServiceName  string
	OTLPEndpoint string
	SampleRatio  float64
	MetricsAddr  string
}

func LoadObservabilityConfig() ObservabilityConfig {
	otlpEndpoint, exists := os.LookupEnv("OBS_OTLP_ENDPOINT")
	if !exists {
		otlpEndpoint = "http://127.0.0.1:4318"
	}
	return ObservabilityConfig{
		Enabled:                 getEnv("OBS_ENABLED", "false"),
		ServiceName:             getEnv("OBS_SERVICE_NAME", "video-support-agent"),
		OTLPEndpoint:            otlpEndpoint,
		SampleRatio:             getEnv("OBS_SAMPLE_RATIO", "1"),
		MetricsAddr:             getEnv("OBS_METRICS_ADDR", "127.0.0.1:9464"),
		MetricsAllowNonLoopback: getEnv("OBS_METRICS_ALLOW_NON_LOOPBACK", "false"),
	}
}

var observabilityServiceName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,63}$`)

func ParseObservabilityConfig(cfg ObservabilityConfig) (ObservabilitySettings, error) {
	var settings ObservabilitySettings

	switch cfg.Enabled {
	case "false":
		return settings, nil
	case "true":
		settings.Enabled = true
	default:
		return settings, errors.New("invalid observability configuration: OBS_ENABLED")
	}

	if !observabilityServiceName.MatchString(cfg.ServiceName) {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_SERVICE_NAME")
	}

	endpoint, err := url.Parse(cfg.OTLPEndpoint)
	if err != nil ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.Hostname() == "" ||
		endpoint.User != nil ||
		endpoint.Opaque != "" ||
		endpoint.RawQuery != "" ||
		endpoint.ForceQuery ||
		endpoint.Fragment != "" ||
		endpoint.RawPath != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_OTLP_ENDPOINT")
	}

	if port := endpoint.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_OTLP_ENDPOINT")
		}
	}

	ratio, err := strconv.ParseFloat(cfg.SampleRatio, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_SAMPLE_RATIO")
	}

	allowNonLoopback := false
	switch cfg.MetricsAllowNonLoopback {
	case "", "false":
	case "true":
		allowNonLoopback = true
	default:
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_METRICS_ALLOW_NON_LOOPBACK")
	}

	host, port, err := net.SplitHostPort(cfg.MetricsAddr)
	if err != nil {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_METRICS_ADDR")
	}

	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || err != nil || number < 1 || number > 65535 {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_METRICS_ADDR")
	}

	if !ip.IsLoopback() && !allowNonLoopback {
		return ObservabilitySettings{}, errors.New("invalid observability configuration: OBS_METRICS_ADDR")
	}

	settings.ServiceName = cfg.ServiceName
	settings.OTLPEndpoint = cfg.OTLPEndpoint
	settings.SampleRatio = ratio
	settings.MetricsAddr = cfg.MetricsAddr
	return settings, nil
}
