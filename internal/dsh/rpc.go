package dsh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"
	"unicode/utf8"
)

const (
	// rpcTimeout 限制单次 RPC 的总耗时，避免慢宿主阻塞调用方。
	rpcTimeout = 5 * time.Second
	// rpcMaxRequestBytes 与 dsh Remote API 约定的请求体上限一致。
	rpcMaxRequestBytes = 1 << 20
	// rpcMaxResponseBytes 限制响应体大小，超大历史直接失败而不是返回半截内容。
	rpcMaxResponseBytes = 4 << 20
	// rpcMaxErrorMessageChars 限制服务端错误消息进入日志/界面的长度。
	rpcMaxErrorMessageChars = 512
	// rpcIDBytes 使用 128 位随机数作为请求关联 ID，避免跨请求碰撞。
	rpcIDBytes = 16
)

var (
	errRPCUnknownEndpoint  = errors.New("不支持的 dsh RPC endpoint")
	errRPCMissingHome      = errors.New("dsh 宿主缺少 Home 目录，拒绝读取相对路径的认证文件")
	errRPCHomeNotAbsolute  = errors.New("dsh 宿主 Home 必须是绝对路径")
	errRPCRequestTooLarge  = errors.New("dsh RPC 请求体超过 1 MiB 限制")
	errRPCResponseTooLarge = errors.New("dsh RPC 响应体超过 4 MiB 限制")
	errRPCMalformed        = errors.New("dsh RPC 响应格式无效")
	errRPCMismatchedID     = errors.New("dsh RPC 响应 rpcId 与请求不匹配")
	errRPCMissingResult    = errors.New("dsh RPC 响应缺少 result")
	errRPCMissingError     = errors.New("dsh RPC 响应 ok=false 但缺少 error")
	errRPCAuth             = errors.New("dsh RPC 认证材料已失效")
)

// allowedEndpoints 是 dsh Remote API 的固定白名单；新增 endpoint 必须同步更新设计文档与测试。
var allowedEndpoints = map[string]struct{}{
	"session/list":         {},
	"session/projections":  {},
	"session/page":         {},
	"session/prompt":       {},
	"session/create":       {},
	"session/rename":       {},
	"userQuestions/answer": {},
	"$events/result":       {},
}

// rpcClient 通过本机回环 HTTP 调用 dsh Remote API。
//
// http、cookie 与 now 均为可替换字段，便于测试注入；生产构造使用
// newRPCClient，每次调用重新生成短期 Cookie，不缓存认证材料。
type rpcClient struct {
	host   Host
	http   *http.Client
	cookie func(string, time.Time) (string, error)
	now    func() time.Time
}

// rpcError 表示 dsh 服务端返回的业务错误。Message 已按长度截断，
// 调用方可通过 errors.As 取得 Code 做分类处理。
type rpcError struct {
	Code    string
	Message string
}

// Error 返回包含错误码的可读信息；不包含 Cookie 或认证材料。
func (e rpcError) Error() string {
	if e.Code == "" {
		return "dsh RPC 错误: " + e.Message
	}
	return "dsh RPC 错误 " + e.Code + ": " + e.Message
}

// rpcRequest 是 dsh Remote API 的客户端请求帧。
type rpcRequest struct {
	Type    string            `json:"type"`
	RPCID   string            `json:"rpcId"`
	Method  string            `json:"method"`
	Payload rpcRequestPayload `json:"payload"`
}

// rpcRequestPayload 固定包含 args 字段；args 为 nil 时序列化为 null。
type rpcRequestPayload struct {
	Args any `json:"args"`
}

// rpcResponse 是 dsh Remote API 的服务端响应帧。
type rpcResponse struct {
	Type   string     `json:"type"`
	RPCID  string     `json:"rpcId"`
	Result *rpcResult `json:"result"`
}

type rpcResult struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error *rpcResultError `json:"error"`
}

type rpcResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// newRPCClient 为宿主构造 RPC 客户端；Cookie 由认证记录实时生成。
func newRPCClient(host Host) *rpcClient {
	return &rpcClient{
		host: host,
		cookie: func(path string, now time.Time) (string, error) {
			authority := fmt.Sprintf("127.0.0.1:%d", host.Port)
			return cookieForCredentials(path, authority, now)
		},
		now: time.Now,
		http: &http.Client{
			Timeout: rpcTimeout,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConnsPerHost: 2,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// call 调用白名单内的 endpoint，并校验响应帧的 type、rpcId 与 result.ok。
//
// args 会作为 payload.args 原样序列化；成功时返回 result.value 的原始 JSON。
// 失败时不重试，错误信息不包含 Cookie 或认证密钥。
func (c *rpcClient) call(ctx context.Context, endpoint string, args any) (json.RawMessage, error) {
	if _, ok := allowedEndpoints[endpoint]; !ok {
		return nil, errRPCUnknownEndpoint
	}
	credentialsPath, err := c.credentialsPath()
	if err != nil {
		return nil, err
	}

	rpcID, err := newRPCID()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(rpcRequest{
		Type:    "client-request",
		RPCID:   rpcID,
		Method:  endpoint,
		Payload: rpcRequestPayload{Args: args},
	})
	if err != nil {
		return nil, errors.New("编码 dsh RPC 请求失败")
	}
	if len(body) > rpcMaxRequestBytes {
		return nil, errRPCRequestTooLarge
	}

	cookie, err := c.cookie(credentialsPath, c.now())
	if err != nil {
		return nil, fmt.Errorf("生成 dsh RPC 认证 Cookie 失败: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.host.URL()+"api/"+endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return nil, errors.New("构造 dsh RPC 请求失败")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", cookie)

	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("发送 dsh RPC 请求失败: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		return nil, errRPCAuth
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dsh RPC HTTP 状态 %d", response.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, rpcMaxResponseBytes+1))
	if err != nil {
		return nil, errors.New("读取 dsh RPC 响应失败")
	}
	if len(data) > rpcMaxResponseBytes {
		return nil, errRPCResponseTooLarge
	}

	var parsed rpcResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, errRPCMalformed
	}
	if parsed.Type != "server-response" {
		return nil, errRPCMalformed
	}
	if parsed.RPCID != rpcID {
		return nil, errRPCMismatchedID
	}
	if parsed.Result == nil {
		return nil, errRPCMissingResult
	}
	if !parsed.Result.OK {
		if parsed.Result.Error == nil {
			return nil, errRPCMissingError
		}
		return nil, &rpcError{
			Code:    parsed.Result.Error.Code,
			Message: truncateRPCMessage(parsed.Result.Error.Message),
		}
	}
	return parsed.Result.Value, nil
}

// credentialsPath 返回认证文件绝对路径；Home 为空或相对路径时失败关闭，
// 避免 filepath.Join 将 ".credentials.yaml" 解析到进程当前目录。
func (c *rpcClient) credentialsPath() (string, error) {
	if c.host.Home == "" {
		return "", errRPCMissingHome
	}
	if !filepath.IsAbs(c.host.Home) {
		return "", errRPCHomeNotAbsolute
	}
	return filepath.Join(c.host.Home, ".credentials.yaml"), nil
}

// newRPCID 生成 16 字节随机数的十六进制表示。
func newRPCID() (string, error) {
	var raw [rpcIDBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("生成 dsh RPC 请求 ID 失败")
	}
	return hex.EncodeToString(raw[:]), nil
}

// truncateRPCMessage 按字符数截断服务端错误消息，避免日志被超长内容淹没。
func truncateRPCMessage(message string) string {
	if utf8.RuneCountInString(message) <= rpcMaxErrorMessageChars {
		return message
	}
	return string([]rune(message)[:rpcMaxErrorMessageChars])
}
