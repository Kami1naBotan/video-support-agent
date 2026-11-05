package eventbus

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
	"video-support-agent/internal/config"
)

// A gated socket proves cancellation happens after the consumer enters broker
// startup I/O, rather than before Run starts. No broker state is simulated.
func TestConsumerStartupCancellationPreservesCause(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	cfg := config.KafkaConfig{Brokers: listener.Addr().String(), ClientID: "startup-cancellation", ConnectTimeout: "1s", RequestTimeout: "2s", DeliveryTimeout: "4s", ShutdownTimeout: "1s"}
	consumer, err := NewConsumer(cfg, "startup-cancellation", TopicDocumentEvents)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- consumer.Run(ctx, func(context.Context, Delivery) error { return nil }) }()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not enter broker startup")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup cancellation lost cause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup cancellation exceeded bound")
	}
}
