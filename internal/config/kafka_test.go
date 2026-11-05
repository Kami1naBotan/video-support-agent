package config

import (
	"strings"
	"testing"
	"time"
)

func kafkaFixture() KafkaConfig {
	return KafkaConfig{Brokers: "127.0.0.1:19092", ClientID: "contract-test", ConnectTimeout: "5s", RequestTimeout: "10s", DeliveryTimeout: "30s", ShutdownTimeout: "30s"}
}

func TestKafkaConfigContract(t *testing.T) {
	for _, brokers := range []string{"127.0.0.1:19092", "[::1]:19092", "kafka:9092,KAFKA-2.example:9093"} {
		t.Run("valid_"+brokers, func(t *testing.T) {
			cfg := kafkaFixture()
			cfg.Brokers = brokers
			got, err := ParseKafkaConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got.Brokers, ",") != brokers || got.ClientID != cfg.ClientID || got.ConnectTimeout != 5*time.Second || got.RequestTimeout != 10*time.Second || got.DeliveryTimeout != 30*time.Second || got.ShutdownTimeout != 30*time.Second {
				t.Fatalf("unexpected settings: %+v", got)
			}
		})
	}
	cases := map[string]func(*KafkaConfig){
		"empty_brokers":     func(c *KafkaConfig) { c.Brokers = "" },
		"missing_port":      func(c *KafkaConfig) { c.Brokers = "localhost" },
		"credentials":       func(c *KafkaConfig) { c.Brokers = "secret@localhost:9092" },
		"scheme":            func(c *KafkaConfig) { c.Brokers = "kafka://localhost:9092" },
		"whitespace":        func(c *KafkaConfig) { c.Brokers = " localhost:9092" },
		"duplicate":         func(c *KafkaConfig) { c.Brokers = "kafka:9092,KAFKA:9092" },
		"too_many":          func(c *KafkaConfig) { c.Brokers = strings.Repeat("localhost:9092,", 8) + "localhost:9093" },
		"zero_port":         func(c *KafkaConfig) { c.Brokers = "localhost:0" },
		"large_port":        func(c *KafkaConfig) { c.Brokers = "localhost:65536" },
		"noncanonical_port": func(c *KafkaConfig) { c.Brokers = "localhost:09092" },
		"bad_hostname":      func(c *KafkaConfig) { c.Brokers = "-host:9092" },
		"empty_client":      func(c *KafkaConfig) { c.ClientID = "" },
		"long_client":       func(c *KafkaConfig) { c.ClientID = strings.Repeat("a", 65) },
		"bad_client":        func(c *KafkaConfig) { c.ClientID = "secret\nvalue" },
		"invalid_duration":  func(c *KafkaConfig) { c.ConnectTimeout = "secret" },
		"short_duration":    func(c *KafkaConfig) { c.ConnectTimeout = "999ms" },
		"long_duration":     func(c *KafkaConfig) { c.ShutdownTimeout = "61s" },
		"connect_order":     func(c *KafkaConfig) { c.ConnectTimeout = "11s" },
		"delivery_order":    func(c *KafkaConfig) { c.DeliveryTimeout = "10s" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := kafkaFixture()
			mutate(&cfg)
			_, err := ParseKafkaConfig(cfg)
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration value leaked")
			}
		})
	}
}

func TestKafkaEnvironmentMapping(t *testing.T) {
	values := map[string]string{"KAFKA_BROKERS": "localhost:29092", "KAFKA_CLIENT_ID": "custom-worker", "KAFKA_CONNECT_TIMEOUT": "2s", "KAFKA_REQUEST_TIMEOUT": "3s", "KAFKA_DELIVERY_TIMEOUT": "4s", "KAFKA_SHUTDOWN_TIMEOUT": "5s"}
	for key, value := range values {
		t.Setenv(key, value)
	}
	got := LoadKafkaConfig()
	want := KafkaConfig{"localhost:29092", "custom-worker", "2s", "3s", "4s", "5s"}
	if got != want {
		t.Fatalf("environment mapping mismatch: %+v", got)
	}
}
