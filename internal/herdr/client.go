package herdr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

const defaultRequestTimeout = 10 * time.Second

// Dialer 定义 Client 建立本地 Socket 连接所需的拨号能力。
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Client 是使用单请求单连接策略的 Herdr 本地 Socket 客户端。
type Client struct {
	socketPath string
	dialer     Dialer
	timeout    time.Duration
	nextID     atomic.Uint64
}

// NewClient 创建使用 socketPath 的 Herdr 客户端。
func NewClient(socketPath string, dialer Dialer, timeout time.Duration) *Client {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &Client{socketPath: socketPath, dialer: dialer, timeout: timeout}
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dialer.DialContext(requestContext, "unix", c.socketPath)
	if err != nil {
		return unavailableError(err)
	}
	defer conn.Close()

	deadline, _ := requestContext.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return unavailableError(err)
	}
	stopClose := context.AfterFunc(requestContext, func() {
		_ = conn.Close()
	})
	defer stopClose()

	request := requestEnvelope{
		ID:     fmt.Sprintf("pal:%d", c.nextID.Add(1)),
		Method: method,
		Params: params,
	}
	if err := writeRequest(conn, request); err != nil {
		return unavailableContextError(requestContext, err)
	}
	line, err := readLine(bufio.NewReader(conn))
	if err != nil {
		if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrProtocol) && !errors.Is(err, io.EOF) {
			return err
		}
		return unavailableContextError(requestContext, err)
	}
	return parseResponse(line, request.ID, result)
}

func unavailableError(err error) error {
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func unavailableContextError(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		err = errors.Join(err, contextError)
	}
	return unavailableError(err)
}
