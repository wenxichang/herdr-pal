package dsh

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/policy"
)

// newHotReloadBackend 构造按发现配置返回宿主的 Backend。
//
// discover 记录每次调用时的配置，便于断言 syncHosts 用的是最新配置。
func newHotReloadBackend(t *testing.T) *Backend {
	t.Helper()
	guard, err := policy.NewGuard("user")
	if err != nil {
		t.Fatalf("NewGuard() 返回错误：%v", err)
	}
	sink := &backendTestSink{}
	discover := func(_ context.Context, cfg DiscoveryConfig) ([]Host, error) {
		if cfg.URL == "" {
			return nil, nil
		}
		host := testValidHost()
		// 显式 URL 模式：从配置推导端口，模拟 dsh 换端口。
		port := explicitPort(cfg.URL)
		host.PID = 0
		host.Port = port
		host.Version = versionExplicit
		host.Generation = hostGeneration("explicit", cfg.URL, cfg.Home)
		return []Host{host}, nil
	}
	factory := func(host Host) (*hostWorker, error) {
		executor := NewExecutor(host, &backendFakeAPI{}, sink, guard)
		executor.logger = backendTestLogger()
		return &hostWorker{host: host, executor: executor}, nil
	}
	backend := newBackend(DiscoveryConfig{URL: "http://127.0.0.1:30001"}, discover, factory, sink, guard, backendTestLogger())
	backend.discoveryInterval = 10 * time.Millisecond
	return backend
}

// explicitPort 从 http://127.0.0.1:<port> 中取出端口，解析失败返回 0。
func explicitPort(raw string) uint16 {
	index := strings.LastIndexByte(raw, ':')
	if index < 0 {
		return 0
	}
	port, err := strconv.ParseUint(raw[index+1:], 10, 16)
	if err != nil {
		return 0
	}
	return uint16(port)
}

// TestUpdateDiscoveryConfigReportsChange 验证等价配置返回 false，变更返回 true。
func TestUpdateDiscoveryConfigReportsChange(t *testing.T) {
	backend := newHotReloadBackend(t)
	original := backend.discoveryConfig()

	changed, err := backend.UpdateDiscoveryConfig(original)
	if err != nil {
		t.Fatalf("UpdateDiscoveryConfig() error = %v", err)
	}
	if changed {
		t.Fatal("相同配置不应报告变更")
	}

	next := original
	next.URL = "http://127.0.0.1:30002"
	changed, err = backend.UpdateDiscoveryConfig(next)
	if err != nil {
		t.Fatalf("UpdateDiscoveryConfig() error = %v", err)
	}
	if !changed {
		t.Fatal("不同配置应报告变更")
	}
	if got := backend.discoveryConfig(); got != next {
		t.Fatalf("discoveryConfig() = %#v, want %#v", got, next)
	}

	// 只改 include_unavailable 也算变更，因为它影响目录内容。
	toggled := next
	toggled.IncludeUnavailable = true
	if changed, err := backend.UpdateDiscoveryConfig(toggled); err != nil || !changed {
		t.Fatalf("切换 include_unavailable：changed=%v err=%v", changed, err)
	}
}

// TestUpdateDiscoveryConfigRejectsInvalidVariant 验证非法变体被拒绝且不改变配置。
func TestUpdateDiscoveryConfigRejectsInvalidVariant(t *testing.T) {
	backend := newHotReloadBackend(t)
	before := backend.discoveryConfig()

	invalid := before
	invalid.Variant = "wsl"
	if _, err := backend.UpdateDiscoveryConfig(invalid); err == nil {
		t.Fatal("非法变体应被拒绝")
	}
	if got := backend.discoveryConfig(); got != before {
		t.Fatalf("拒绝后配置不应改变：%#v", got)
	}
}

// TestBackendHotReloadSwapsHost 验证端口变化后旧宿主被撤下、新宿主被建立。
func TestBackendHotReloadSwapsHost(t *testing.T) {
	backend := newHotReloadBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = backend.Run(ctx)
		close(done)
	}()

	waitBackend(t, "初始宿主就绪", func() bool {
		return len(backend.CurrentTargets()) == 1
	})
	first := backend.discoveryConfig()
	if _, err := backend.UpdateDiscoveryConfig(DiscoveryConfig{
		URL:     "http://127.0.0.1:30002",
		Variant: first.Variant,
	}); err != nil {
		t.Fatalf("UpdateDiscoveryConfig() error = %v", err)
	}

	// 端口变化会形成新 hostRef；旧宿主必须被撤下，目录里只剩一个目标。
	waitBackend(t, "旧宿主被新宿主替换", func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		if len(backend.hosts) != 1 {
			return false
		}
		for _, worker := range backend.hosts {
			return worker.host.Port == 30002
		}
		return false
	})

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Backend 未随 ctx 退出")
	}
}

// TestUpdateDiscoveryConfigIsSafeDuringRun 验证运行中并发热更新不触发数据竞争。
func TestUpdateDiscoveryConfigIsSafeDuringRun(t *testing.T) {
	backend := newHotReloadBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = backend.Run(ctx) }()

	ports := []string{"http://127.0.0.1:30003", "http://127.0.0.1:30004", "http://127.0.0.1:30005"}
	for round := 0; round < 20; round++ {
		if _, err := backend.UpdateDiscoveryConfig(DiscoveryConfig{
			URL:     ports[round%len(ports)],
			Variant: variantWeb,
		}); err != nil {
			t.Fatalf("第 %d 轮热更新失败：%v", round, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 最终配置必须与最后一次提交一致。
	if got := backend.discoveryConfig().URL; got != ports[(20-1)%len(ports)] {
		t.Fatalf("最终配置 = %q", got)
	}
}
