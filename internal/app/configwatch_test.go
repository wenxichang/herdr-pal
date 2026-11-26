package app

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/dsh"
)

// fakeConfigUpdater 记录配置监听提交的发现配置。
type fakeConfigUpdater struct {
	mu    sync.Mutex
	calls int
	// updates 只记录成功接受的配置；报错时不自增。
	updates []dsh.DiscoveryConfig
	err     error
	// changed 为 false 时模拟“配置等价、无需变更”。
	changed bool
}

func (u *fakeConfigUpdater) UpdateDiscoveryConfig(cfg dsh.DiscoveryConfig) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	if u.err != nil {
		return false, u.err
	}
	u.updates = append(u.updates, cfg)
	return u.changed, nil
}

// last 返回最近一次提交的配置；没有记录时返回 false。
func (u *fakeConfigUpdater) last() (dsh.DiscoveryConfig, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.updates) == 0 {
		return dsh.DiscoveryConfig{}, false
	}
	return u.updates[len(u.updates)-1], true
}

// count 返回成功接受配置的次数。
func (u *fakeConfigUpdater) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.updates)
}

// callCount 返回包含失败在内的总调用次数。
func (u *fakeConfigUpdater) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// writeWatchConfig 在 dir 下写入内容固定的客户端配置并返回路径。
func writeWatchConfig(t *testing.T, dir, dshURL string, includeUnavailable bool) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	raw := fmt.Sprintf(
		`{"relay":{"url":"wss://relay.internal:9443","key":%q,"skip_verify":true},`+
			`"herdr":{"disabled":true},`+
			`"dsh":{"url":%q,"home":"/tmp/dsh-home","variant":"web","include_unavailable":%t}}`,
		testRelayKey(t), dshURL, includeUnavailable,
	)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("写入测试配置失败：%v", err)
	}
	return path
}

// withFastWatchInterval 把轮询周期缩短到 10ms，并在测试结束时恢复。
func withFastWatchInterval(t *testing.T) {
	t.Helper()
	previous := configWatchInterval
	configWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { configWatchInterval = previous })
}

// startConfigWatch 在后台运行监听，返回可取消的上下文与结束信号。
//
// 启动后短暂等待，确保监听已经读取并记录基线摘要；否则紧接着的写文件可能
// 被当成基线，导致“无变化”的误判。
func startConfigWatch(t *testing.T, path string, updater dshConfigUpdater) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watchDSHConfig(ctx, path, updater, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(cancel)
	// 至少跨越两个轮询周期，保证基线读取完成。
	time.Sleep(configWatchInterval * 3)
	return cancel, done
}

// waitUpdates 在期限内等待成功接受的次数达到 want，返回是否达成。
func waitUpdates(updater *fakeConfigUpdater, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if updater.count() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return updater.count() >= want
}

// waitCalls 在期限内等待总调用次数达到 want，用于包含失败的场景。
func waitCalls(updater *fakeConfigUpdater, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if updater.callCount() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return updater.callCount() >= want
}

// TestConfigDigestDetectsContentChange 验证摘要只随内容变化，且与 MD5 一致。
func TestConfigDigestDetectsContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("alpha"), 0o600); err != nil {
		t.Fatalf("写入文件失败：%v", err)
	}

	first, err := configDigest(path)
	if err != nil {
		t.Fatalf("configDigest() error = %v", err)
	}
	sum := md5.Sum([]byte("alpha"))
	if want := hex.EncodeToString(sum[:]); first != want {
		t.Fatalf("configDigest() = %q, want MD5 %q", first, want)
	}

	// 仅修改 mtime 不应改变摘要。
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("修改时间戳失败：%v", err)
	}
	unchanged, err := configDigest(path)
	if err != nil {
		t.Fatalf("configDigest() error = %v", err)
	}
	if unchanged != first {
		t.Fatalf("仅修改 mtime 后摘要变化：%q != %q", unchanged, first)
	}

	if err := os.WriteFile(path, []byte("beta"), 0o600); err != nil {
		t.Fatalf("改写文件失败：%v", err)
	}
	second, err := configDigest(path)
	if err != nil {
		t.Fatalf("configDigest() error = %v", err)
	}
	if second == first {
		t.Fatal("内容变化后摘要不应相同")
	}
}

// TestConfigDigestReportsMissingAndOversized 验证缺失文件与超大文件的处理。
func TestConfigDigestReportsMissingAndOversized(t *testing.T) {
	if _, err := configDigest(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("缺失文件应返回错误")
	}

	// 超过上限的文件只读取上限内的内容：摘要仍可计算且稳定。
	path := filepath.Join(t.TempDir(), "big.json")
	payload := make([]byte, configMaxBytes+10)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("写入大文件失败：%v", err)
	}
	first, err := configDigest(path)
	if err != nil {
		t.Fatalf("configDigest() error = %v", err)
	}
	second, err := configDigest(path)
	if err != nil {
		t.Fatalf("configDigest() error = %v", err)
	}
	if first != second {
		t.Fatal("同一文件两次摘要应一致")
	}
}

// TestWatchDSHConfigAppliesChanges 验证配置变化后提交新的发现配置。
func TestWatchDSHConfigAppliesChanges(t *testing.T) {
	withFastWatchInterval(t)
	dir := t.TempDir()
	path := writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	updater := &fakeConfigUpdater{changed: true}
	startConfigWatch(t, path, updater)

	// 首轮只记录基线，不应提交。
	if updater.count() != 0 {
		t.Fatalf("基线轮次不应提交配置，实际 %d 次", updater.count())
	}

	writeWatchConfig(t, dir, "http://127.0.0.1:10002", true)
	if !waitUpdates(updater, 1) {
		t.Fatal("配置变化后未提交新配置")
	}
	got, _ := updater.last()
	want := dsh.DiscoveryConfig{URL: "http://127.0.0.1:10002", Home: "/tmp/dsh-home", Variant: "web", IncludeUnavailable: true}
	if got != want {
		t.Fatalf("提交的配置 = %#v, want %#v", got, want)
	}
}

// TestWatchDSHConfigIgnoresIdenticalContent 验证内容未变时不重复提交。
func TestWatchDSHConfigIgnoresIdenticalContent(t *testing.T) {
	withFastWatchInterval(t)
	dir := t.TempDir()
	path := writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	updater := &fakeConfigUpdater{changed: true}
	startConfigWatch(t, path, updater)

	// 重写相同内容（内容摘要不变）不应触发提交。
	writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	time.Sleep(100 * time.Millisecond)
	if updater.count() != 0 {
		t.Fatalf("相同内容不应提交配置，实际 %d 次", updater.count())
	}
}

// TestWatchDSHConfigRetriesInvalidJSON 验证半截 JSON 不会退出，内容稳定后应用。
func TestWatchDSHConfigRetriesInvalidJSON(t *testing.T) {
	withFastWatchInterval(t)
	dir := t.TempDir()
	path := writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	updater := &fakeConfigUpdater{changed: true}
	_, done := startConfigWatch(t, path, updater)

	// 模拟编辑器截断写入：只写半截 JSON。
	if err := os.WriteFile(path, []byte(`{"relay":{"url":`), 0o600); err != nil {
		t.Fatalf("写入半截配置失败：%v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if updater.count() != 0 {
		t.Fatalf("无效配置不应提交，实际 %d 次", updater.count())
	}
	select {
	case err := <-done:
		t.Fatalf("无效配置不应让监听退出：%v", err)
	default:
	}

	// 写入完整内容后应成功应用。
	writeWatchConfig(t, dir, "http://127.0.0.1:10003", false)
	if !waitUpdates(updater, 1) {
		t.Fatal("内容修复后未提交配置")
	}
	got, _ := updater.last()
	if got.URL != "http://127.0.0.1:10003" {
		t.Fatalf("提交的 URL = %q", got.URL)
	}
}

// TestWatchDSHConfigKeepsRunningAfterUpdaterError 验证接收方报错时继续监听。
func TestWatchDSHConfigKeepsRunningAfterUpdaterError(t *testing.T) {
	withFastWatchInterval(t)
	dir := t.TempDir()
	path := writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	updater := &fakeConfigUpdater{changed: true, err: errors.New("后端已停止")}
	_, done := startConfigWatch(t, path, updater)

	writeWatchConfig(t, dir, "http://127.0.0.1:10004", false)
	waitCalls(updater, 1)
	select {
	case err := <-done:
		t.Fatalf("接收方报错不应让监听退出：%v", err)
	default:
	}
	if updater.count() != 0 {
		t.Fatalf("接收方报错时不应记录成功配置，实际 %d 次", updater.count())
	}

	// 恢复正常后，再次变更应能被应用。
	updater.mu.Lock()
	updater.err = nil
	updater.mu.Unlock()
	writeWatchConfig(t, dir, "http://127.0.0.1:10005", false)
	if !waitUpdates(updater, 1) {
		t.Fatal("恢复后变更未提交")
	}
	got, _ := updater.last()
	if got.URL != "http://127.0.0.1:10005" {
		t.Fatalf("提交的 URL = %q, want 10005", got.URL)
	}
}

// TestWatchDSHConfigSkipsWhenBackendReportsNoChange 验证接收方判定无变化时不重复提交。
func TestWatchDSHConfigSkipsWhenBackendReportsNoChange(t *testing.T) {
	withFastWatchInterval(t)
	dir := t.TempDir()
	path := writeWatchConfig(t, dir, "http://127.0.0.1:10001", false)
	updater := &fakeConfigUpdater{changed: false}
	startConfigWatch(t, path, updater)

	writeWatchConfig(t, dir, "http://127.0.0.1:10006", false)
	if !waitUpdates(updater, 1) {
		t.Fatal("应至少提交一次由接收方判定")
	}
	// 摘要已推进，同一内容不应再次提交。
	time.Sleep(80 * time.Millisecond)
	if updater.count() != 1 {
		t.Fatalf("内容未再变化时不应重复提交，实际 %d 次", updater.count())
	}
}

// TestWatchDSHConfigStopsOnContextCancel 验证 ctx 取消后监听及时退出。
func TestWatchDSHConfigStopsOnContextCancel(t *testing.T) {
	withFastWatchInterval(t)
	path := writeWatchConfig(t, t.TempDir(), "http://127.0.0.1:10001", false)
	cancel, done := startConfigWatch(t, path, &fakeConfigUpdater{changed: true})

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("监听退出错误 = %v，期望 context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后监听未退出")
	}
}

// TestWatchDSHConfigRejectsInvalidArguments 验证参数校验。
func TestWatchDSHConfigRejectsInvalidArguments(t *testing.T) {
	if err := watchDSHConfig(nil, "x", &fakeConfigUpdater{}, nil); err == nil {
		t.Fatal("缺少 context 应报错")
	}
	if err := watchDSHConfig(context.Background(), "x", nil, nil); err == nil {
		t.Fatal("缺少接收方应报错")
	}
}

// TestWatchDSHConfigMissingFileKeepsRunning 验证启动时文件缺失不导致退出。
func TestWatchDSHConfigMissingFileKeepsRunning(t *testing.T) {
	withFastWatchInterval(t)
	path := filepath.Join(t.TempDir(), "config.json")
	updater := &fakeConfigUpdater{changed: true}
	_, done := startConfigWatch(t, path, updater)

	// 文件稍后出现时应能被读取并提交。
	writeWatchConfig(t, filepath.Dir(path), "http://127.0.0.1:10007", false)
	if !waitUpdates(updater, 1) {
		t.Fatal("文件出现后未提交配置")
	}
	select {
	case err := <-done:
		t.Fatalf("监听不应退出：%v", err)
	default:
	}
}

// TestNewDSHConfigWatchNeverFails 验证监听组件对上层永远返回 nil。
func TestNewDSHConfigWatchNeverFails(t *testing.T) {
	withFastWatchInterval(t)
	// updater 返回错误时也只在日志中体现，组件本身不报错。
	watch := newDSHConfigWatch(filepath.Join(t.TempDir(), "missing.json"), &fakeConfigUpdater{err: errors.New("boom")},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watch(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("监听组件应返回 nil，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("监听组件未随 ctx 退出")
	}
}

// TestRunRelayComponentsAuxiliaryExitIsIgnored 验证附属组件结束不拖垮关键组件。
//
// 两个关键组件都阻塞到 ctx 取消：这样附属组件退出是唯一先发生的事件，
// 可用于确认它不会触发整体收尾。
func TestRunRelayComponentsAuxiliaryExitIsIgnored(t *testing.T) {
	auxExited := make(chan struct{})

	blockUntilDone := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	aux := func(ctx context.Context) error {
		close(auxExited)
		return fmt.Errorf("附属组件故意失败")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		err, _ := runRelayComponents(ctx, blockUntilDone, blockUntilDone, time.Second, aux)
		done <- err
	}()

	select {
	case <-auxExited:
	case <-time.After(2 * time.Second):
		t.Fatal("附属组件未启动")
	}
	// 附属组件已失败，关键组件仍在等待：整体不应提前结束。
	select {
	case err := <-done:
		t.Fatalf("附属组件失败不应结束整体运行：%v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("整体退出错误 = %v，期望 context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后未及时退出")
	}
}

// TestRunRelayComponentsPropagatesPrimaryFailure 验证关键组件失败仍被上报。
func TestRunRelayComponentsPropagatesPrimaryFailure(t *testing.T) {
	failure := errors.New("relay 崩溃")
	runRelay := func(context.Context) error { return failure }
	runSupervisor := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	// 附属组件立即返回，用来覆盖“附属先结束”的读取顺序。
	aux := func(context.Context) error { return nil }

	err, _ := runRelayComponents(context.Background(), runRelay, runSupervisor, time.Second, aux)
	var componentErr *relayComponentError
	if !errors.As(err, &componentErr) || !errors.Is(err, failure) {
		t.Fatalf("错误 = %v，期望携带原始失败", err)
	}
	if componentErr.component != "relay_connection" {
		t.Fatalf("失败组件 = %q，want relay_connection", componentErr.component)
	}
	if !strings.Contains(err.Error(), "relay 崩溃") {
		t.Fatalf("错误信息缺少原始原因：%v", err)
	}
}
