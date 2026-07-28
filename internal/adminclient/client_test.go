package adminclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestClientSessionSupportsSequentialRequests(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	client := newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) { return clientSide, nil })
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverSide)
		for index := 1; index <= 2; index++ {
			frame, err := adminproto.ReadFrame(reader)
			if err != nil {
				serverDone <- err
				return
			}
			request, err := adminproto.DecodeRequest(frame)
			if err != nil || request.ID != "request-"+string(rune('0'+index)) {
				serverDone <- errors.New("unexpected request")
				return
			}
			response, _ := adminproto.NewResultResponse(request.ID, adminproto.ServerStopResult{Stopping: index == 2})
			encoded, _ := adminproto.EncodeResponse(response)
			if _, err := serverSide.Write(encoded); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	defer serverSide.Close()
	session, err := client.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for index := 1; index <= 2; index++ {
		var result adminproto.ServerStopResult
		if err := session.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Stopping != (index == 2) {
			t.Fatalf("result %d = %#v", index, result)
		}
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestClientClassifiesBusinessProtocolAndTransportErrors(t *testing.T) {
	t.Run("business", func(t *testing.T) {
		client := clientWithServer(t, func(request adminproto.Request) []byte {
			response, _ := adminproto.NewErrorResponse(request.ID, adminproto.Error{Code: adminproto.CodeCredentialNotFound, Message: "Key 不存在"})
			encoded, _ := adminproto.EncodeResponse(response)
			return encoded
		})
		var result adminproto.CredentialResult
		err := client.Call(t.Context(), adminproto.MethodKeyShow, adminproto.CredentialIDParams{CredentialID: 9}, &result)
		var serverErr *ServerError
		if !errors.As(err, &serverErr) || serverErr.Code != adminproto.CodeCredentialNotFound {
			t.Fatalf("Call() error = %v", err)
		}
	})

	t.Run("response id", func(t *testing.T) {
		client := clientWithServer(t, func(adminproto.Request) []byte {
			response, _ := adminproto.NewResultResponse("different-id", adminproto.EmptyParams{})
			encoded, _ := adminproto.EncodeResponse(response)
			return encoded
		})
		err := client.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Call() error = %v, want ErrProtocol", err)
		}
	})

	t.Run("response protocol", func(t *testing.T) {
		client := clientWithServer(t, func(request adminproto.Request) []byte {
			return []byte(`{"protocol":"HPAP/2","id":"` + request.ID + `","result":{}}` + "\n")
		})
		err := client.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Call() error = %v, want ErrProtocol", err)
		}
	})

	t.Run("server protocol error", func(t *testing.T) {
		client := clientWithServer(t, func(request adminproto.Request) []byte {
			response, _ := adminproto.NewErrorResponse(request.ID, adminproto.Error{Code: adminproto.CodeProtocolUnsupportedVersion, Message: "版本不兼容"})
			encoded, _ := adminproto.EncodeResponse(response)
			return encoded
		})
		err := client.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Call() error = %v, want ErrProtocol", err)
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		client := clientWithServer(t, func(adminproto.Request) []byte {
			return []byte(strings.Repeat("x", adminproto.MaxFrameBytes+1) + "\n")
		})
		err := client.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Call() error = %v, want ErrProtocol", err)
		}
	})
}

func TestClientEnforcesDialReadAndWriteTimeouts(t *testing.T) {
	t.Run("dial", func(t *testing.T) {
		client, err := New(Config{SocketPath: "/unused", Timeout: 20 * time.Millisecond, Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Open(t.Context()); !errors.Is(err, ErrTransport) {
			t.Fatalf("Open() error = %v", err)
		}
	})

	t.Run("read", func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		client, err := New(Config{SocketPath: "/unused", Timeout: 20 * time.Millisecond, Dial: func(context.Context, string) (net.Conn, error) { return clientSide, nil }})
		if err != nil {
			t.Fatal(err)
		}
		session, err := client.Open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		go io.Copy(io.Discard, serverSide)
		err = session.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrTransport) {
			t.Fatalf("Call() error = %v", err)
		}
	})

	t.Run("write", func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		client, err := New(Config{SocketPath: "/unused", Timeout: 20 * time.Millisecond, Dial: func(context.Context, string) (net.Conn, error) { return clientSide, nil }})
		if err != nil {
			t.Fatal(err)
		}
		session, err := client.Open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		err = session.Call(t.Context(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &adminproto.ServerStatusResult{})
		if !errors.Is(err, ErrTransport) {
			t.Fatalf("Call() error = %v", err)
		}
	})
}

func TestClientReportsUnavailableUnixSocketAndUsesFiveSecondDefault(t *testing.T) {
	client, err := New(Config{SocketPath: filepath.Join(t.TempDir(), "missing.sock")})
	if err != nil {
		t.Fatal(err)
	}
	if client.timeout != 5*time.Second {
		t.Fatalf("default timeout = %s", client.timeout)
	}
	if _, err := client.Open(t.Context()); !errors.Is(err, ErrTransport) {
		t.Fatalf("Open() error = %v", err)
	}
}

func newPipeAdminClient(t *testing.T, dial DialFunc) *Client {
	t.Helper()
	var next atomic.Int32
	client, err := New(Config{
		SocketPath: "/unused", Timeout: time.Second, Dial: dial,
		RequestID: func() string { return "request-" + string(rune('0'+next.Add(1))) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func clientWithServer(t *testing.T, respond func(adminproto.Request) []byte) *Client {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { _ = serverSide.Close() })
	go func() {
		frame, err := adminproto.ReadFrame(bufio.NewReader(serverSide))
		if err != nil {
			return
		}
		request, err := adminproto.DecodeRequest(frame)
		if err != nil {
			return
		}
		_, _ = serverSide.Write(respond(request))
	}()
	return newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) { return clientSide, nil })
}
