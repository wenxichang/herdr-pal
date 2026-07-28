package credential

import (
	"context"
	"net/http"
	"strings"
)

// Verifier 把 Bearer Key 解析为服务端可信终端身份。
type Verifier interface {
	VerifyBearer(context.Context, string) (Identity, error)
}

// VerifyRequest 从 HTTP Authorization 头读取 Bearer Key 并完成身份验证。
func VerifyRequest(request *http.Request, verifier Verifier) (Identity, error) {
	if request == nil || verifier == nil {
		return Identity{}, ErrUnauthenticated
	}
	parts := strings.Fields(request.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Identity{}, ErrUnauthenticated
	}
	identity, err := verifier.VerifyBearer(request.Context(), parts[1])
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	return identity, nil
}
