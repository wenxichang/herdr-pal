// Package adminclient 实现 hp-cli 使用的 HPAP 本地客户端。
package adminclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

const defaultTimeout = 5 * time.Second

var (
	// ErrConfig 表示本地客户端配置无效。
	ErrConfig = errors.New("HPAP 客户端配置错误")
	// ErrTransport 表示 Admin Socket 连接、读写或超时失败。
	ErrTransport = errors.New("HPAP 传输错误")
	// ErrProtocol 表示 HPAP 响应不兼容、无法关联或超过限制。
	ErrProtocol = errors.New("HPAP 协议错误")
	// ErrUnsupported 表示当前平台不支持 Unix Admin Socket。
	ErrUnsupported = errors.New("当前平台不支持 HPAP Unix Socket")
)

// DialFunc 建立一条到 Admin Socket 的本地连接。
type DialFunc func(context.Context, string) (net.Conn, error)

// Config 指定 Admin Socket、固定超时和测试注入点。
type Config struct {
	SocketPath string
	Timeout    time.Duration
	Dial       DialFunc
	RequestID  func() string
	Now        func() time.Time
}

// ServerError 保存 Server 返回的稳定业务错误码。
type ServerError struct {
	Code    adminproto.ErrorCode
	Message string
	Details json.RawMessage
}

// Error 返回适合 CLI 展示的稳定业务错误。
func (err *ServerError) Error() string {
	if err == nil {
		return "HPAP Server 错误"
	}
	if strings.TrimSpace(err.Message) == "" {
		return string(err.Code)
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

type classifiedError struct {
	kind  error
	cause error
}

func (err *classifiedError) Error() string {
	if err == nil || err.kind == nil {
		return "HPAP 操作失败"
	}
	if err.cause == nil {
		return err.kind.Error()
	}
	return fmt.Sprintf("%s: %v", err.kind, err.cause)
}

func (err *classifiedError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.kind
}

func (err *classifiedError) Is(target error) bool {
	return err != nil && (target == err.kind || errors.Is(err.cause, target))
}

// Client 每次命令建立一条本地连接；分页方法会在同一连接内发出多个顺序请求。
type Client struct {
	socketPath string
	timeout    time.Duration
	dial       DialFunc
	requestID  func() string
	now        func() time.Time
}

// Session 是一条支持多个顺序 HPAP 请求的本地连接。
type Session struct {
	connection net.Conn
	reader     *bufio.Reader
	timeout    time.Duration
	requestID  func() string
	now        func() time.Time

	mu sync.Mutex
}

// New 创建 HPAP 本地客户端。
func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.SocketPath) == "" {
		return nil, ErrConfig
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.Dial == nil {
		config.Dial = defaultDial
	}
	if config.RequestID == nil {
		var sequence atomic.Uint64
		prefix := fmt.Sprintf("hp-%d", time.Now().UnixNano())
		config.RequestID = func() string { return fmt.Sprintf("%s-%d", prefix, sequence.Add(1)) }
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Client{
		socketPath: config.SocketPath, timeout: config.Timeout, dial: config.Dial,
		requestID: config.RequestID, now: config.Now,
	}, nil
}

// Open 建立一条可复用到当前 CLI 命令结束的 HPAP 会话。
func (client *Client) Open(ctx context.Context) (*Session, error) {
	if client == nil {
		return nil, ErrConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dialContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	connection, err := client.dial(dialContext, client.socketPath)
	if err != nil {
		return nil, transportError(err)
	}
	return &Session{
		connection: connection, reader: bufio.NewReaderSize(connection, 64*1024),
		timeout: client.timeout, requestID: client.requestID, now: client.now,
	}, nil
}

// Call 为单个非分页命令建立连接、执行请求并关闭连接。
func (client *Client) Call(ctx context.Context, method adminproto.Method, params, result any) error {
	session, err := client.Open(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Call(ctx, method, params, result)
}

// Call 在当前连接中顺序执行一条请求。
func (session *Session) Call(ctx context.Context, method adminproto.Method, params, result any) error {
	if session == nil || session.connection == nil {
		return ErrConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	paramsJSON := json.RawMessage(`{}`)
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return protocolError(err)
		}
		paramsJSON = encoded
	}
	request := adminproto.Request{Protocol: adminproto.Protocol, ID: session.requestID(), Method: method, Params: paramsJSON}
	encoded, err := adminproto.EncodeRequest(request)
	if err != nil {
		return protocolError(err)
	}
	deadline := session.callDeadline(ctx)
	if err := session.connection.SetWriteDeadline(deadline); err != nil {
		return transportError(err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = session.connection.SetDeadline(session.now()) })
	defer stopCancellation()
	if err := writeAll(session.connection, encoded); err != nil {
		return transportError(err)
	}
	if err := session.connection.SetReadDeadline(deadline); err != nil {
		return transportError(err)
	}
	frame, err := adminproto.ReadFrame(session.reader)
	if err != nil {
		if adminproto.IsCode(err, adminproto.CodeProtocolLimitExceeded) {
			return protocolError(err)
		}
		return transportError(err)
	}
	response, err := adminproto.DecodeResponse(frame)
	if err != nil {
		return protocolError(err)
	}
	if response.ID != request.ID {
		return protocolError(errors.New("HPAP 响应 ID 不匹配"))
	}
	if response.Error != nil {
		serverError := &ServerError{Code: response.Error.Code, Message: response.Error.Message, Details: append(json.RawMessage(nil), response.Error.Details...)}
		if strings.HasPrefix(string(serverError.Code), "protocol.") {
			return protocolError(serverError)
		}
		return serverError
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return protocolError(err)
	}
	return nil
}

// Close 关闭当前本地连接。
func (session *Session) Close() error {
	if session == nil || session.connection == nil {
		return nil
	}
	return session.connection.Close()
}

func (session *Session) callDeadline(ctx context.Context) time.Time {
	deadline := session.now().Add(session.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		return contextDeadline
	}
	return deadline
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[written:]
	}
	return nil
}

func transportError(cause error) error { return &classifiedError{kind: ErrTransport, cause: cause} }
func protocolError(cause error) error  { return &classifiedError{kind: ErrProtocol, cause: cause} }
