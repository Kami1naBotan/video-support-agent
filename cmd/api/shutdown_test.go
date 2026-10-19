package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestShutdownHTTPServerWaitsForActiveRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	shutdownStarted := make(chan struct{})

	var releaseOnce sync.Once
	releaseResponse := func() {
		releaseOnce.Do(func() {
			close(allowResponse)
		})
	}

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			close(requestStarted)

			select {
			case <-allowResponse:
				_, _ = io.WriteString(w, "completed")
			case <-r.Context().Done():
				return
			}
		},
	))

	server.Config.RegisterOnShutdown(func() {
		close(shutdownStarted)
	})

	defer server.Close()
	defer releaseResponse()

	client := server.Client()
	client.Timeout = 5 * time.Second

	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get(server.URL)
		if err != nil {
			requestResult <- err
			return
		}
		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		if err != nil {
			requestResult <- err
			return
		}
		if response.StatusCode != http.StatusOK ||
			string(body) != "completed" {
			requestResult <- fmt.Errorf(
				"unexpected response: status=%d body=%q",
				response.StatusCode,
				string(body),
			)
			return
		}

		requestResult <- nil
	}()

	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the handler")
	}

	shutdownResult := make(chan error, 1)
	go func() {
		shutdownResult <- shutdownHTTPServer(server.Config, 5*time.Second)
	}()

	select {
	case <-shutdownStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not start")
	}

	// 请求仍被阻塞，此时关闭不应已经完成。
	select {
	case err := <-shutdownResult:
		t.Fatalf("shutdown returned before request finished: %v", err)
	default:
	}

	releaseResponse()

	select {
	case err := <-requestResult:
		if err != nil {
			t.Fatalf("active request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete")
	}

	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("graceful shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}

func TestShutdownHTTPServerTimeoutClosesActiveRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	handlerCanceled := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			close(requestStarted)

			// 模拟尚未完成、但能够响应取消的耗时操作。
			<-r.Context().Done()
			close(handlerCanceled)
		},
	))
	defer server.Close()

	client := server.Client()
	client.Timeout = 5 * time.Second

	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Get(server.URL)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()

	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the handler")
	}

	err := shutdownHTTPServer(server.Config, 100*time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected shutdown deadline error, got %v", err)
	}

	select {
	case <-handlerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("forced close must cancel the active request context")
	}

	select {
	case requestErr := <-requestResult:
		if requestErr == nil {
			t.Fatal("unfinished request should fail after forced close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP client did not observe the closed connection")
	}
}
