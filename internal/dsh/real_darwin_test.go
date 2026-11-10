//go:build darwin

package dsh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// realDSHTestTimeout 限制整次只读核对的总耗时；Read 可能多次分页。
const realDSHTestTimeout = 60 * time.Second

// TestRealDSHHostReadOnly 对真实运行中的本机 dsh 宿主执行只读核对。
//
// 默认跳过，只有显式设置 HERDR_PAL_REAL_DSH=1 才运行。测试只调用宿主发现、
// session/list 与 session/page，不调用 Prompt、Create、SubmitAnswer 或任何
// 写接口；输出只包含宿主数量和成功/失败，不输出会话标题、正文、ID 或认证材料。
func TestRealDSHHostReadOnly(t *testing.T) {
	if os.Getenv("HERDR_PAL_REAL_DSH") != "1" {
		t.Skip("设置 HERDR_PAL_REAL_DSH=1 后运行")
	}

	ctx, cancel := context.WithTimeout(context.Background(), realDSHTestTimeout)
	defer cancel()

	hosts, err := discoverHosts(ctx, DiscoveryConfig{}, systemProber{}, "darwin")
	if err != nil {
		t.Fatalf("dsh 宿主发现失败：%s", safeRealDSHError(err))
	}
	if len(hosts) == 0 {
		t.Fatal("未发现本机 dsh 宿主；请先启动 dsh web 或 Desktop 后重试")
	}
	t.Logf("发现 %d 个 dsh 宿主", len(hosts))

	// 丢弃客户端日志；真实宿主日志不进入测试输出。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for index, host := range hosts {
		client := NewClient(host, logger)
		sessions, err := client.Sessions(ctx)
		if err != nil {
			t.Fatalf("宿主 %d 会话目录读取失败：%s", index+1, safeRealDSHError(err))
		}
		t.Logf("宿主 %d 会话目录读取成功", index+1)
		if len(sessions) == 0 {
			continue
		}

		body, err := client.Read(ctx, sessions[0].ID)
		if err != nil {
			t.Fatalf("宿主 %d 首会话正文读取失败：%s", index+1, safeRealDSHError(err))
		}
		t.Logf("宿主 %d 首会话正文读取成功（%d 字节）", index+1, len(body))
	}
}

// safeRealDSHError 把真实宿主的错误归类为固定文案。
//
// 真实宿主返回的会话 ID 或服务端消息可能进入原始错误，因此测试输出只保留
// 错误类别和经清洗的 RPC 错误码，避免标题、正文、ID 或认证材料外泄。
func safeRealDSHError(err error) string {
	var rpcErr *rpcError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errRPCAuth):
		return "认证被拒绝（401），Cookie 或认证记录不兼容"
	case errors.As(err, &rpcErr):
		return "dsh RPC 业务错误（code=" + safeRPCErrorCode(rpcErr.Code) + "）"
	case errors.Is(err, errCredentialFile),
		errors.Is(err, errCredentialSize),
		errors.Is(err, errCredentialFormat),
		errors.Is(err, errCredentialVersion),
		errors.Is(err, errCredentialSecret):
		return "dsh 认证记录不兼容或权限不正确"
	case errors.Is(err, errRPCMalformed),
		errors.Is(err, errRPCMismatchedID),
		errors.Is(err, errRPCMissingResult),
		errors.Is(err, errRPCMissingError):
		return "dsh RPC 响应格式不兼容"
	case errors.Is(err, errRPCResponseTooLarge):
		return "dsh RPC 响应超过大小上限"
	case errors.Is(err, errHistoryUnavailable),
		errors.Is(err, errHistoryTooLarge),
		errors.Is(err, errPageMalformed):
		return "dsh 会话历史不可完整读取"
	default:
		return "请求失败（详情已省略）"
	}
}

// safeRPCErrorCode 只保留 RPC 错误码中的字母、数字、下划线和连字符，
// 防止服务端消息被当作错误码带入测试输出。
func safeRPCErrorCode(code string) string {
	const maxCodeLength = 64
	var builder strings.Builder
	for _, r := range code {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	if builder.Len() > maxCodeLength {
		return builder.String()[:maxCodeLength]
	}
	return builder.String()
}
