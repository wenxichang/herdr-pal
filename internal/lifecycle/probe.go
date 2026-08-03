package lifecycle

import (
	"context"
	"errors"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

// ErrInvalidProbe 表示 Herdr 存活探测器缺少公开 API 客户端。
var ErrInvalidProbe = errors.New("Herdr 存活探测器无效")

// ProbeResult 描述一次 Herdr 公共 API 探测得到的存活和兼容性结论。
type ProbeResult struct {
	Alive      bool
	Compatible bool
}

// Probe 提供 Supervisor 所需的低成本存活探测和首次完整就绪校验。
type Probe interface {
	Probe(context.Context) (ProbeResult, error)
	VerifyReady(context.Context) error
}

// ProbeHerdr 是存活探测所需的最小 Herdr 公共 API 集合。
type ProbeHerdr interface {
	CheckCompatible(context.Context) error
	Snapshot(context.Context) (herdr.Snapshot, error)
}

// PublicProbe 只通过 Herdr 公共 ping 和 session.snapshot 判断 Server 生命周期。
type PublicProbe struct {
	client ProbeHerdr
}

// NewPublicProbe 创建 Herdr 公共 API 存活探测器。
func NewPublicProbe(client ProbeHerdr) (*PublicProbe, error) {
	if client == nil {
		return nil, ErrInvalidProbe
	}
	return &PublicProbe{client: client}, nil
}

// Probe 使用公开 ping 区分不可连接和协议不兼容。
func (probe *PublicProbe) Probe(ctx context.Context) (ProbeResult, error) {
	if probe == nil || probe.client == nil || ctx == nil {
		return ProbeResult{}, ErrInvalidProbe
	}
	err := probe.client.CheckCompatible(ctx)
	if err == nil {
		return ProbeResult{Alive: true, Compatible: true}, nil
	}
	if errors.Is(err, herdr.ErrProtocolMismatch) {
		return ProbeResult{Alive: true, Compatible: false}, nil
	}
	return ProbeResult{}, err
}

// VerifyReady 读取一次权威会话快照，确认兼容 Server 已完成公共 API 初始化。
func (probe *PublicProbe) VerifyReady(ctx context.Context) error {
	if probe == nil || probe.client == nil || ctx == nil {
		return ErrInvalidProbe
	}
	_, err := probe.client.Snapshot(ctx)
	return err
}
