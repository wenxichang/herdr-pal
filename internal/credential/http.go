package credential

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Verifier 把 Bearer Key 和真实 TCP 来源解析为服务端可信终端身份。
type Verifier interface {
	VerifyBearer(context.Context, string, netip.Addr) (Identity, error)
}

// VerifyRequest 从 HTTP Authorization 和 RemoteAddr 完成身份及来源验证。
//
// Forwarded 与 X-Forwarded-For 等代理头不参与安全决策。
func VerifyRequest(request *http.Request, verifier Verifier) (Identity, error) {
	if request == nil || verifier == nil {
		return Identity{}, ErrUnauthenticated
	}
	parts := strings.Fields(request.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Identity{}, ErrUnauthenticated
	}
	source, err := RequestSourceAddr(request)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	identity, err := verifier.VerifyBearer(request.Context(), parts[1], source)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return identity, nil
}

// RequestSourceAddr 返回 HTTP 连接真实 RemoteAddr 中的规范化来源 IP。
func RequestSourceAddr(request *http.Request) (netip.Addr, error) {
	if request == nil {
		return netip.Addr{}, ErrUnauthenticated
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err != nil {
		return netip.Addr{}, ErrUnauthenticated
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, ErrUnauthenticated
	}
	return normalizeSourceAddr(address), nil
}
