package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type KafkaConfig struct {
	Brokers         string
	ClientID        string
	ConnectTimeout  string
	RequestTimeout  string
	DeliveryTimeout string
	ShutdownTimeout string
}

type KafkaSettings struct {
	Brokers         []string
	ClientID        string
	ConnectTimeout  time.Duration
	RequestTimeout  time.Duration
	DeliveryTimeout time.Duration
	ShutdownTimeout time.Duration
}

func LoadKafkaConfig() KafkaConfig {
	return KafkaConfig{
		Brokers:         getEnv("KAFKA_BROKERS", "127.0.0.1:19092"),
		ClientID:        getEnv("KAFKA_CLIENT_ID", "knowledge-worker"),
		ConnectTimeout:  getEnv("KAFKA_CONNECT_TIMEOUT", "5s"),
		RequestTimeout:  getEnv("KAFKA_REQUEST_TIMEOUT", "10s"),
		DeliveryTimeout: getEnv("KAFKA_DELIVERY_TIMEOUT", "30s"),
		ShutdownTimeout: getEnv("KAFKA_SHUTDOWN_TIMEOUT", "30s"),
	}
}

func ParseKafkaConfig(cfg KafkaConfig) (KafkaSettings, error) {
	var result KafkaSettings

	invalid := func(field string) (KafkaSettings, error) {
		return KafkaSettings{}, fmt.Errorf("invalid Kafka configuration: %s", field)
	}

	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`).MatchString(cfg.ClientID) {
		return invalid("KAFKA_CLIENT_ID")
	}
	result.ClientID = cfg.ClientID

	seen := make(map[string]struct{})
	brokers := strings.Split(cfg.Brokers, ",")
	if len(brokers) > 8 {
		return invalid("KAFKA_BROKERS")
	}

	for _, broker := range brokers {
		if broker != strings.TrimSpace(broker) || strings.ContainsAny(broker, "/@?#\\\t\r\n ") {
			return invalid("KAFKA_BROKERS")
		}

		host, port, err := net.SplitHostPort(broker)
		if err != nil || host == "" {
			return invalid("KAFKA_BROKERS")
		}

		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return invalid("KAFKA_BROKERS")
		}

		if net.ParseIP(host) == nil && !validKafkaHostname(host) {
			return invalid("KAFKA_BROKERS")
		}

		key := strings.ToLower(broker)
		if _, exists := seen[key]; exists {
			return invalid("KAFKA_BROKERS: duplicate broker")
		}
		seen[key] = struct{}{}
		result.Brokers = append(result.Brokers, broker)
	}

	fields := []struct {
		name string
		raw  string
		dst  *time.Duration
	}{
		{
			"KAFKA_CONNECT_TIMEOUT",
			cfg.ConnectTimeout,
			&result.ConnectTimeout,
		},
		{
			"KAFKA_REQUEST_TIMEOUT",
			cfg.RequestTimeout,
			&result.RequestTimeout,
		},
		{
			"KAFKA_DELIVERY_TIMEOUT",
			cfg.DeliveryTimeout,
			&result.DeliveryTimeout,
		},
		{
			"KAFKA_SHUTDOWN_TIMEOUT",
			cfg.ShutdownTimeout,
			&result.ShutdownTimeout,
		},
	}

	for _, field := range fields {
		value, err := time.ParseDuration(field.raw)
		if err != nil || value < time.Second || value > time.Minute {
			return invalid(field.name)
		}
		*field.dst = value
	}

	if result.ConnectTimeout > result.RequestTimeout || result.RequestTimeout >= result.DeliveryTimeout {
		return invalid("require connect_timeout <= request_timeout < delivery_timeout")
	}

	return result, nil
}

func validKafkaHostname(host string) bool {
	if len(host) > 253 {
		return false
	}

	label := regexp.MustCompile(
		`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`,
	)
	for _, part := range strings.Split(host, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}
