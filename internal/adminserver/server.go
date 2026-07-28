package adminserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

const (
	defaultMaxConnections = 32
	defaultRequestTimeout = 5 * time.Second
)

var errInvalidServerConfig = errors.New("HPAP Admin Server 配置无效")

// Handler 顺序处理一条已经通过 HPAP 信封校验的管理请求。
type Handler interface {
	Handle(context.Context, adminproto.Request) (HandleResult, error)
}

// HandlerFunc 允许普通函数作为 HPAP 请求处理器。
type HandlerFunc func(context.Context, adminproto.Request) (HandleResult, error)

// Handle 调用当前函数处理请求。
func (handler HandlerFunc) Handle(ctx context.Context, request adminproto.Request) (HandleResult, error) {
	return handler(ctx, request)
}

// HandleResult 包含唯一响应、可审计目标和响应写完后才允许执行的动作。
type HandleResult struct {
	Response    adminproto.Response
	AuditTarget string
	AfterWrite  func()
}

// ServerConfig 指定 HPAP 请求处理器、审计日志和连接资源限制。
type ServerConfig struct {
	Handler        Handler
	Logger         *slog.Logger
	MaxConnections int
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
}

// Server 使用有界连接并发，并在每条连接内顺序处理 HPAP 请求。
type Server struct {
	handler        Handler
	logger         *slog.Logger
	maxConnections int
	readTimeout    time.Duration
	writeTimeout   time.Duration
}

// NewServer 创建 HPAP 管理服务核心。
func NewServer(config ServerConfig) (*Server, error) {
	if config.Handler == nil || config.Logger == nil {
		return nil, errInvalidServerConfig
	}
	if config.MaxConnections <= 0 {
		config.MaxConnections = defaultMaxConnections
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = defaultRequestTimeout
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaultRequestTimeout
	}
	return &Server{
		handler: config.Handler, logger: config.Logger, maxConnections: config.MaxConnections,
		readTimeout: config.ReadTimeout, writeTimeout: config.WriteTimeout,
	}, nil
}

// Serve 接受多条管理连接，直到 context 取消或监听器不可恢复地失败。
func (server *Server) Serve(ctx context.Context, listener net.Listener) error {
	if server == nil || listener == nil {
		return errInvalidServerConfig
	}
	if ctx == nil {
		ctx = context.Background()
	}
	semaphore := make(chan struct{}, server.maxConnections)
	var connections sync.WaitGroup
	stopWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopWatcher:
		}
	}()
	defer close(stopWatcher)
	defer connections.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("接受 HPAP 管理连接: %w", err)
		}
		select {
		case semaphore <- struct{}{}:
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer func() { <-semaphore }()
				server.serveConnection(ctx, connection)
			}()
		default:
			auditBusyConnection(server.logger, asNetPeerUID(connection))
			_ = connection.Close()
		}
	}
}

func (server *Server) serveConnection(ctx context.Context, connection net.Conn) {
	if connection == nil {
		return
	}
	defer connection.Close()
	connectionDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-connectionDone:
		}
	}()
	defer close(connectionDone)
	reader := bufio.NewReaderSize(connection, 64*1024)
	writer := bufio.NewWriterSize(connection, 64*1024)
	peerUID := peerUIDOf(asNetPeerUID(connection))
	for {
		startedAt := time.Now()
		if err := connection.SetReadDeadline(startedAt.Add(server.readTimeout)); err != nil {
			return
		}
		frame, err := adminproto.ReadFrame(reader)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			code := codecErrorCode(err)
			auditProtocolFailure(server.logger, peerUID, adminproto.Request{}, code, time.Since(startedAt))
			return
		}
		request, err := adminproto.DecodeRequest(frame)
		if err != nil {
			code, message := codecError(err)
			response, responseErr := adminproto.NewErrorResponse(request.ID, adminproto.Error{Code: code, Message: message})
			if responseErr != nil {
				auditProtocolFailure(server.logger, peerUID, request, code, time.Since(startedAt))
				return
			}
			written, _, writeErr := server.writeResponse(connection, writer, request.ID, response)
			if writeErr != nil {
				auditProtocolFailure(server.logger, peerUID, request, code, time.Since(startedAt))
				return
			}
			auditRequest(server.logger, peerUID, request, "", written, time.Since(startedAt))
			continue
		}

		result, handlerErr := server.handler.Handle(ctx, request)
		if handlerErr != nil {
			server.logger.Error("HPAP Handler 内部失败", "method", request.Method, "request_hash", auditRequestHash(request.ID), "error_type", fmt.Sprintf("%T", handlerErr), "reason", "Handler 返回内部错误")
			result = HandleResult{Response: internalErrorResponse(request.ID)}
		}
		written, fallback, writeErr := server.writeResponse(connection, writer, request.ID, result.Response)
		if writeErr != nil {
			auditRequest(server.logger, peerUID, request, result.AuditTarget, internalErrorResponse(request.ID), time.Since(startedAt))
			return
		}
		auditRequest(server.logger, peerUID, request, result.AuditTarget, written, time.Since(startedAt))
		if result.AfterWrite != nil && handlerErr == nil && !fallback {
			action := result.AfterWrite
			go runAfterWrite(server.logger, request, action)
		}
	}
}

func (server *Server) writeResponse(connection net.Conn, writer *bufio.Writer, requestID string, response adminproto.Response) (adminproto.Response, bool, error) {
	fallback := response.ID != requestID
	encoded, err := adminproto.EncodeResponse(response)
	if err != nil || fallback {
		response = internalErrorResponse(requestID)
		fallback = true
		encoded, err = adminproto.EncodeResponse(response)
		if err != nil {
			return adminproto.Response{}, true, err
		}
	}
	if err := connection.SetWriteDeadline(time.Now().Add(server.writeTimeout)); err != nil {
		return adminproto.Response{}, fallback, err
	}
	if _, err := writer.Write(encoded); err != nil {
		return adminproto.Response{}, fallback, err
	}
	if err := writer.Flush(); err != nil {
		return adminproto.Response{}, fallback, err
	}
	return response, fallback, nil
}

func internalErrorResponse(requestID string) adminproto.Response {
	response, err := adminproto.NewErrorResponse(requestID, adminproto.Error{Code: adminproto.CodeServerInternal, Message: "服务端处理请求失败"})
	if err != nil {
		return adminproto.Response{}
	}
	return response
}

func codecError(err error) (adminproto.ErrorCode, string) {
	var codecErr *adminproto.CodecError
	if errors.As(err, &codecErr) {
		return codecErr.Code, codecErr.Message
	}
	return adminproto.CodeProtocolInvalidRequest, "HPAP 请求无效"
}

func codecErrorCode(err error) adminproto.ErrorCode {
	code, _ := codecError(err)
	return code
}

func runAfterWrite(logger *slog.Logger, request adminproto.Request, action func()) {
	defer func() {
		if recover() != nil && logger != nil {
			logger.Error("HPAP 响应后动作异常", "method", request.Method, "request_hash", auditRequestHash(request.ID), "error_type", "panic")
		}
	}()
	action()
}

type netPeerUID interface {
	PeerUID() uint32
}

func asNetPeerUID(connection net.Conn) netPeerUID {
	peer, _ := connection.(netPeerUID)
	return peer
}

func peerUIDOf(connection netPeerUID) uint32 {
	if connection == nil {
		return 0
	}
	return connection.PeerUID()
}
