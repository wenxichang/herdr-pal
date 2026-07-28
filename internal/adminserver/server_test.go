package adminserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestAdminServerProcessesSequentialRequestsAndKeepsBusinessErrorsOpen(t *testing.T) {
	server := newTestAdminServer(t, HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
		if request.ID == "req-1" {
			response, err := adminproto.NewErrorResponse(request.ID, adminproto.Error{Code: adminproto.CodeCredentialNotFound, Message: "Key 不存在"})
			return HandleResult{Response: response}, err
		}
		response, err := adminproto.NewResultResponse(request.ID, struct {
			Status string `json:"status"`
		}{Status: "ok"})
		return HandleResult{Response: response}, err
	}))
	client, done := startAdminConnection(t, server)
	defer client.Close()
	reader := bufio.NewReader(client)
	writeAdminRequest(t, client, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: adminproto.MethodKeyShow})
	first := readAdminResponse(t, reader)
	writeAdminRequest(t, client, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-2", Method: adminproto.MethodServerStatus})
	second := readAdminResponse(t, reader)
	if first.ID != "req-1" || first.Error == nil || first.Error.Code != adminproto.CodeCredentialNotFound {
		t.Fatalf("first response = %#v", first)
	}
	if second.ID != "req-2" || second.Error != nil || !bytes.Contains(second.Result, []byte(`"status":"ok"`)) {
		t.Fatalf("second response = %#v", second)
	}
	client.Close()
	awaitConnectionDone(t, done)
}

func TestAdminServerHandlesConnectionsConcurrentlyAndRejectsBeyondLimit(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	handler := HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		response, err := adminproto.NewResultResponse(request.ID, struct{}{})
		return HandleResult{Response: response}, err
	})
	server, err := NewServer(ServerConfig{Handler: handler, Logger: discardLogger(), MaxConnections: 2, ReadTimeout: time.Second, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	listener := newChannelListener()
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx, listener) }()

	clients := make([]net.Conn, 0, 3)
	for index := 1; index <= 2; index++ {
		serverSide, clientSide := net.Pipe()
		clients = append(clients, clientSide)
		listener.push(withPeerUID(serverSide, 1000))
		writeAdminRequest(t, clientSide, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-" + string(rune('0'+index)), Method: adminproto.MethodServerStatus})
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("two connections did not run concurrently")
		}
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum active handlers = %d, want 2", maximum.Load())
	}

	thirdServer, thirdClient := net.Pipe()
	clients = append(clients, thirdClient)
	listener.push(withPeerUID(thirdServer, 1000))
	if err := thirdClient.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := thirdClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection beyond limit remained open")
	}

	close(release)
	for _, client := range clients[:2] {
		readAdminResponse(t, bufio.NewReader(client))
	}
	for _, client := range clients {
		client.Close()
	}
	cancel()
	select {
	case err := <-serveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not stop")
	}
}

func TestAdminServerCorrelatesProtocolErrorAndRunsActionAfterWrite(t *testing.T) {
	var writeCompleted atomic.Bool
	actionResult := make(chan bool, 1)
	server := newTestAdminServer(t, HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
		response, err := adminproto.NewResultResponse(request.ID, struct{}{})
		return HandleResult{Response: response, AfterWrite: func() { actionResult <- writeCompleted.Load() }}, err
	}))
	serverSide, clientSide := net.Pipe()
	tracked := &writeTrackingConn{Conn: withPeerUID(serverSide, 1000), completed: &writeCompleted}
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), tracked)
		close(done)
	}()
	defer clientSide.Close()
	if _, err := io.WriteString(clientSide, `{"protocol":"HPAP/2","id":"req-version","method":"server.status"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(clientSide)
	protocolResponse := readAdminResponse(t, reader)
	if protocolResponse.ID != "req-version" || protocolResponse.Error == nil || protocolResponse.Error.Code != adminproto.CodeProtocolUnsupportedVersion {
		t.Fatalf("protocol response = %#v", protocolResponse)
	}
	writeCompleted.Store(false)
	writeAdminRequest(t, clientSide, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-action", Method: adminproto.MethodServerStop})
	response := readAdminResponse(t, reader)
	if response.ID != "req-action" || response.Error != nil {
		t.Fatalf("action response = %#v", response)
	}
	select {
	case afterWrite := <-actionResult:
		if !afterWrite {
			t.Fatal("AfterWrite ran before response write completed")
		}
	case <-time.After(time.Second):
		t.Fatal("AfterWrite did not run")
	}
	clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not stop")
	}
}

func TestAdminServerClosesOversizedFrameAndBoundsOversizedResponse(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		var handlerCalled atomic.Bool
		server := newTestAdminServer(t, HandlerFunc(func(context.Context, adminproto.Request) (HandleResult, error) {
			handlerCalled.Store(true)
			return HandleResult{}, nil
		}))
		client, done := startAdminConnection(t, server)
		writeDone := make(chan error, 1)
		go func() {
			_, err := io.WriteString(client, strings.Repeat("x", adminproto.MaxFrameBytes+1)+"\n")
			writeDone <- err
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("oversized request did not close connection")
		}
		client.Close()
		<-writeDone
		if handlerCalled.Load() {
			t.Fatal("oversized request reached handler")
		}
	})

	t.Run("response", func(t *testing.T) {
		server := newTestAdminServer(t, HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
			response, err := adminproto.NewResultResponse(request.ID, struct {
				Value string `json:"value"`
			}{Value: strings.Repeat("x", adminproto.MaxFrameBytes)})
			if err != nil {
				t.Fatal(err)
			}
			return HandleResult{Response: response}, nil
		}))
		client, done := startAdminConnection(t, server)
		writeAdminRequest(t, client, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-large", Method: adminproto.MethodServerStatus})
		response := readAdminResponse(t, bufio.NewReader(client))
		if response.Error == nil || response.Error.Code != adminproto.CodeServerInternal {
			t.Fatalf("oversized response fallback = %#v", response)
		}
		client.Close()
		awaitConnectionDone(t, done)
	})
}

func TestAdminServerUsesFiveSecondDefaultsAndAppliesDeadlines(t *testing.T) {
	server, err := NewServer(ServerConfig{Handler: HandlerFunc(func(_ context.Context, request adminproto.Request) (HandleResult, error) {
		response, resultErr := adminproto.NewResultResponse(request.ID, struct{}{})
		return HandleResult{Response: response}, resultErr
	}), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if server.readTimeout != 5*time.Second || server.writeTimeout != 5*time.Second {
		t.Fatalf("default timeouts = %s/%s", server.readTimeout, server.writeTimeout)
	}
	server.readTimeout = 100 * time.Millisecond
	server.writeTimeout = 100 * time.Millisecond
	serverSide, clientSide := net.Pipe()
	tracked := &deadlineTrackingConn{Conn: withPeerUID(serverSide, 1000)}
	done := make(chan struct{})
	go func() { server.serveConnection(context.Background(), tracked); close(done) }()
	writeAdminRequest(t, clientSide, adminproto.Request{Protocol: adminproto.Protocol, ID: "req-deadline", Method: adminproto.MethodServerStatus})
	readAdminResponse(t, bufio.NewReader(clientSide))
	clientSide.Close()
	awaitSignal(t, done, "deadline connection")
	if tracked.readDeadlines.Load() == 0 || tracked.writeDeadlines.Load() == 0 {
		t.Fatalf("deadline calls = read:%d write:%d", tracked.readDeadlines.Load(), tracked.writeDeadlines.Load())
	}
}

func newTestAdminServer(t *testing.T, handler Handler) *Server {
	t.Helper()
	server, err := NewServer(ServerConfig{Handler: handler, Logger: discardLogger(), ReadTimeout: time.Second, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func startAdminConnection(t *testing.T, server *Server) (net.Conn, <-chan struct{}) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		server.serveConnection(context.Background(), withPeerUID(serverSide, 1000))
		close(done)
	}()
	return clientSide, done
}

func writeAdminRequest(t *testing.T, writer io.Writer, request adminproto.Request) {
	t.Helper()
	encoded, err := adminproto.EncodeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(encoded); err != nil {
		t.Fatal(err)
	}
}

func readAdminResponse(t *testing.T, reader *bufio.Reader) adminproto.Response {
	t.Helper()
	frame, err := adminproto.ReadFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := adminproto.DecodeResponse(frame)
	if err != nil {
		t.Fatalf("DecodeResponse(%s) error = %v", frame, err)
	}
	return response
}

func awaitConnectionDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	awaitSignal(t, done, "admin connection")
}

func awaitSignal(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s did not stop", label)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type peerUIDWrapper struct {
	net.Conn
	uid uint32
}

func withPeerUID(connection net.Conn, uid uint32) net.Conn {
	return &peerUIDWrapper{Conn: connection, uid: uid}
}

func (connection *peerUIDWrapper) PeerUID() uint32 { return connection.uid }

type writeTrackingConn struct {
	net.Conn
	completed *atomic.Bool
}

func (connection *writeTrackingConn) Write(data []byte) (int, error) {
	written, err := connection.Conn.Write(data)
	if err == nil && written == len(data) {
		connection.completed.Store(true)
	}
	return written, err
}

type deadlineTrackingConn struct {
	net.Conn
	readDeadlines  atomic.Int32
	writeDeadlines atomic.Int32
}

func (connection *deadlineTrackingConn) SetReadDeadline(deadline time.Time) error {
	connection.readDeadlines.Add(1)
	return connection.Conn.SetReadDeadline(deadline)
}

func (connection *deadlineTrackingConn) SetWriteDeadline(deadline time.Time) error {
	connection.writeDeadlines.Add(1)
	return connection.Conn.SetWriteDeadline(deadline)
}

type channelListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func newChannelListener() *channelListener {
	return &channelListener{connections: make(chan net.Conn, 8), closed: make(chan struct{})}
}

func (listener *channelListener) push(connection net.Conn) { listener.connections <- connection }

func (listener *channelListener) Accept() (net.Conn, error) {
	select {
	case connection := <-listener.connections:
		return connection, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}

func (listener *channelListener) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return nil
}

func (listener *channelListener) Addr() net.Addr { return testAddr("admin") }

type testAddr string

func (address testAddr) Network() string { return "test" }
func (address testAddr) String() string  { return string(address) }
