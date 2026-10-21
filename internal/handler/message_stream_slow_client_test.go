package handler

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"video-support-agent/internal/model"
	"video-support-agent/internal/service"
)

// 将一条内存连接交给真实的 net/http Server。
type streamPipeListener struct {
	connection chan net.Conn
	closed     chan struct{}
	closeOnce  sync.Once
}

func newStreamPipeListener(conn net.Conn) *streamPipeListener {
	listener := &streamPipeListener{
		connection: make(chan net.Conn, 1),
		closed:     make(chan struct{}),
	}
	listener.connection <- conn
	return listener
}

func (l *streamPipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connection:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *streamPipeListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func (l *streamPipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

// 只观察真实连接的写错误，不主动制造超时。
type streamObservedConn struct {
	net.Conn
	writeErrors chan error
}

func (c *streamObservedConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	if err != nil {
		select {
		case c.writeErrors <- err:
		default:
		}
	}
	return n, err
}

func TestMessageHandlerSlowClientStartWriteTimeout(t *testing.T) {
	runSlowClientWriteTimeoutTest(t, false)
}

func TestMessageHandlerSlowClientTokenWriteTimeout(t *testing.T) {
	runSlowClientWriteTimeoutTest(t, true)
}

func runSlowClientWriteTimeoutTest(t *testing.T, readStart bool) {
	t.Helper()

	messages := &handlerStreamSaveStore{}
	conversations := &testConversationStore{
		conversation: &model.Conversation{ID: 1, UserID: 7},
	}
	chat := &transportStreamChat{}

	svc := service.NewMessageService(
		messages,
		conversations,
		nil,
		nil,
		chat,
	)
	handler := NewMessageHandler(svc)

	router, token := newMessageTestRouter(t)
	router.POST(
		"/api/v1/conversations/:id/messages/stream",
		handler.CreateStream,
	)

	serverConn, clientConn := net.Pipe()
	writeErrors := make(chan error, 1)

	observedConn := &streamObservedConn{
		Conn:        serverConn,
		writeErrors: writeErrors,
	}
	listener := newStreamPipeListener(observedConn)

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	serveDone := make(chan error, 1)

	writeTimeout := 200 * time.Millisecond
	if readStart {
		// 给接收响应头和 start 留出调度余量。
		writeTimeout = time.Second
	}

	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		WriteTimeout:      writeTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(handlerStarted)
			defer close(handlerDone)
			router.ServeHTTP(w, r)
		}),
	}

	go func() {
		serveDone <- server.Serve(listener)
	}()

	t.Cleanup(func() {
		// 验收断言执行完后，才主动关闭客户端连接。
		_ = clientConn.Close()
		_ = serverConn.Close()
		_ = server.Close()

		select {
		case err := <-serveDone:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("unexpected server exit: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("HTTP server did not exit during cleanup")
		}
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.test/api/v1/conversations/1/messages/stream",
		strings.NewReader(`{"content":"测试问题"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)

	// 仅限制客户端发送请求，防止测试在请求写入阶段挂住。
	if err := clientConn.SetWriteDeadline(
		time.Now().Add(2 * time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := request.Write(clientConn); err != nil {
		t.Fatalf("send request: %v", err)
	}

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	if readStart {
		// 客户端只接收响应头和完整 start，随后停止读取。
		if err := clientConn.SetReadDeadline(
			time.Now().Add(3 * time.Second),
		); err != nil {
			t.Fatal(err)
		}

		response, err := http.ReadResponse(
			bufio.NewReader(clientConn),
			request,
		)
		if err != nil {
			t.Fatalf("read response headers: %v", err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200, got %d", response.StatusCode)
		}

		wantStart := "event: start\n" +
			"data: {\"conversation_id\":1}\n\n"

		startFrame := make([]byte, len(wantStart))
		if _, err := io.ReadFull(response.Body, startFrame); err != nil {
			t.Fatalf("read start event: %v", err)
		}
		if string(startFrame) != wantStart {
			t.Fatalf("unexpected start event: %q", string(startFrame))
		}

		// 不继续读取，也不在这里关闭 Body。
		// 底层连接统一由 t.Cleanup 关闭。
	}

	// readStart=false：start 的发送阻塞。
	// readStart=true：后续 token 的发送阻塞。
	// 两个场景都必须由服务端写超时打断，而非客户端主动关闭。
	select {
	case err := <-writeErrors:
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("expected a network write timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked write was not interrupted by the server timeout")
	}

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after write timeout")
	}

	// Handler 退出后再检查替身，避免并发读写。
	if messages.saveCalls != 0 || messages.created != nil {
		t.Fatal("interrupted request must not save messages")
	}

	if !readStart {
		if chat.calls != 0 {
			t.Fatalf(
				"failed start delivery must not start the model, got %d calls",
				chat.calls,
			)
		}
		return
	}

	if chat.calls != 1 {
		t.Fatalf("expected one model call, got %d", chat.calls)
	}

	var callbackNetErr net.Error
	if !errors.As(chat.callbackErr, &callbackNetErr) ||
		!callbackNetErr.Timeout() {
		t.Fatalf(
			"model callback must receive the write timeout: %v",
			chat.callbackErr,
		)
	}

	if !errors.Is(chat.ctxAtFailure, context.Canceled) {
		t.Fatalf(
			"write failure must cancel the model context immediately: %v",
			chat.ctxAtFailure,
		)
	}

	// 复用同一个 Service，验证之前的会话占用已释放。
	// 第二次请求使用正常的 Recorder，不复用超时连接。
	nextRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/conversations/1/messages/stream",
		strings.NewReader(`{"content":"再次发送"}`),
	)
	nextRequest.Header.Set("Content-Type", "application/json")
	nextRequest.Header.Set("Accept", "text/event-stream")
	nextRequest.Header.Set("Authorization", "Bearer "+token)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, nextRequest)

	if recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), "event: done\n") {
		t.Fatalf(
			"same conversation must remain usable: status=%d body=%s",
			recorder.Code,
			recorder.Body.String(),
		)
	}

	if chat.calls != 2 {
		t.Fatalf(
			"expected another model call, got %d total calls",
			chat.calls,
		)
	}
	if messages.saveCalls != 1 ||
		messages.created == nil ||
		messages.created.Content != "再次发送" {
		t.Fatal("only the second, successful request should save")
	}
}
