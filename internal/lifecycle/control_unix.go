//go:build !windows

package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	controlFrameLimit = 4 * 1024
	controlIOTimeout  = 2 * time.Second
)

type unixControlServer struct{}

type unixControlClient struct{}

func newPlatformControlServer() StatusServer { return unixControlServer{} }

func newPlatformControlClient() StatusClient { return unixControlClient{} }

func (unixControlServer) Run(ctx context.Context, path string, current func() Status) error {
	if ctx == nil || strings.TrimSpace(path) == "" || current == nil {
		return ErrControlProtocol
	}
	directory := filepath.Dir(path)
	if err := prepareControlDirectory(directory); err != nil {
		return err
	}
	if err := removeStaleControlSocket(path); err != nil {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrControlUnavailable, err)
	}
	defer listener.Close()
	defer os.Remove(path)
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("设置 Pal 本地健康端点权限: %w", err)
	}
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()

	var handlers sync.WaitGroup
	defer handlers.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("%w: %v", ErrControlUnavailable, err)
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			handleControlConnection(ctx, connection, current)
		}()
	}
}

func prepareControlDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("创建 Pal 本地健康目录: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("检查 Pal 本地健康目录: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 本地健康目录不是普通目录", ErrControlUnavailable)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("设置 Pal 本地健康目录权限: %w", err)
	}
	return nil
}

func removeStaleControlSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 Pal 本地健康端点: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: 本地健康端点路径已被非 Socket 文件占用", ErrControlUnavailable)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("清理旧 Pal 本地健康端点: %w", err)
	}
	return nil
}

func handleControlConnection(ctx context.Context, connection net.Conn, current func() Status) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(controlIOTimeout))
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()

	reader := bufio.NewReader(io.LimitReader(connection, controlFrameLimit+1))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > controlFrameLimit {
		_ = writeControlResponse(connection, ControlResponse{Error: "请求帧无效"})
		return
	}
	var request ControlRequest
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Method) == "" {
		_ = writeControlResponse(connection, ControlResponse{Error: "请求 JSON 无效"})
		return
	}
	if request.Method != controlMethodStatus {
		_ = writeControlResponse(connection, ControlResponse{Error: "方法不受支持"})
		return
	}
	status := current()
	_ = writeControlResponse(connection, ControlResponse{Status: &status})
}

func writeControlResponse(destination io.Writer, response ControlResponse) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = destination.Write(encoded)
	return err
}

func (unixControlClient) Status(ctx context.Context, path string) (Status, error) {
	if ctx == nil || strings.TrimSpace(path) == "" {
		return Status{}, ErrControlProtocol
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrControlUnavailable, err)
	}
	defer connection.Close()
	deadline := time.Now().Add(controlIOTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	if err := writeControlRequest(connection, ControlRequest{Method: controlMethodStatus}); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrControlUnavailable, err)
	}
	line, err := bufio.NewReader(io.LimitReader(connection, controlFrameLimit+1)).ReadBytes('\n')
	if err != nil || len(line) > controlFrameLimit {
		return Status{}, ErrControlProtocol
	}
	var response ControlResponse
	decoder := json.NewDecoder(strings.NewReader(string(line)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return Status{}, fmt.Errorf("%w: %v", ErrControlProtocol, err)
	}
	if response.Error != "" {
		return Status{}, fmt.Errorf("%w: %s", ErrControlProtocol, response.Error)
	}
	if response.Status == nil {
		return Status{}, ErrControlProtocol
	}
	return *response.Status, nil
}

func writeControlRequest(destination io.Writer, request ControlRequest) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = destination.Write(encoded)
	return err
}
