package serverapp

import (
	"errors"
	"log/slog"
	"os"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/server"
	"github.com/wenxichang/herdr-pal/internal/version"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

var errInvalidRuntimeDependency = errors.New("服务运行状态依赖无效")

// RuntimeLogger 保存配置基础级别，并允许管理面临时启用或关闭 debug。
type RuntimeLogger struct {
	Logger    *slog.Logger
	baseLevel slog.Level
	level     slog.LevelVar
}

// BaseLevel 返回配置文件声明的基础日志级别。
func (logger *RuntimeLogger) BaseLevel() slog.Level {
	if logger == nil {
		return slog.LevelInfo
	}
	return logger.baseLevel
}

// CurrentLevel 返回当前实际生效的日志级别。
func (logger *RuntimeLogger) CurrentLevel() slog.Level {
	if logger == nil {
		return slog.LevelInfo
	}
	return logger.level.Level()
}

// DebugEnabled 报告当前实际级别是否允许 debug 日志。
func (logger *RuntimeLogger) DebugEnabled() bool {
	return logger != nil && logger.CurrentLevel() <= slog.LevelDebug
}

// EnableDebug 立即把实际日志级别切换为 debug。
func (logger *RuntimeLogger) EnableDebug() {
	if logger != nil {
		logger.level.Set(slog.LevelDebug)
	}
}

// DisableDebug 立即把实际日志级别恢复为配置基础级别。
func (logger *RuntimeLogger) DisableDebug() {
	if logger != nil {
		logger.level.Set(logger.baseLevel)
	}
}

// RuntimeConfig 提供服务运行状态中不属于动态组件快照的元数据。
type RuntimeConfig struct {
	StartedAt   time.Time
	Now         func() time.Time
	PID         int
	GOOS        string
	GOARCH      string
	Version     string
	Commit      string
	BuiltAt     string
	RelayListen string
	AdminSocket string
	TLS         server.TLSInfo
	Stop        func()
}

// CredentialCounts 是按当前时间互斥分类的凭据数量。
type CredentialCounts struct {
	Enabled  int
	Disabled int
	Expired  int
}

// RuntimeStatus 是 HPAP 管理面可安全展示的服务运行快照。
type RuntimeStatus struct {
	ObservedAt      time.Time
	StartedAt       time.Time
	Uptime          time.Duration
	Version         string
	Commit          string
	BuiltAt         string
	PID             int
	GOOS            string
	GOARCH          string
	HPAP            string
	HPRP            string
	RelayListen     string
	AdminSocket     string
	TLS             server.TLSInfo
	WeCom           wecom.StatusSnapshot
	DebugEnabled    bool
	BaseLogLevel    string
	PrincipalCount  int
	ConnectionCount int
	SessionCount    int
	Credentials     CredentialCounts
}

// WeComStatusProvider 提供不包含凭据和消息正文的企业微信状态快照。
type WeComStatusProvider interface {
	Status() wecom.StatusSnapshot
}

// ConnectionSnapshotProvider 提供当前 HPRP 连接快照。
type ConnectionSnapshotProvider interface {
	Connections() []server.ConnectionView
}

// SessionSnapshotProvider 提供不改变路由状态的 Agent 会话快照。
type SessionSnapshotProvider interface {
	ManagementSessions(server.SessionFilter) []server.SessionView
}

// CredentialSnapshotProvider 提供不包含明文 Key 的凭据记录快照。
type CredentialSnapshotProvider interface {
	List() []credential.Record
}

// RuntimeInspector 聚合线程安全快照，并提供动态日志与一次性停止入口。
type RuntimeInspector struct {
	config      RuntimeConfig
	logger      *RuntimeLogger
	weCom       WeComStatusProvider
	connections ConnectionSnapshotProvider
	sessions    SessionSnapshotProvider
	credentials CredentialSnapshotProvider
	stopOnce    sync.Once
}

// NewRuntimeInspector 创建只读聚合器；空的进程元数据会使用当前构建和运行环境补齐。
func NewRuntimeInspector(config RuntimeConfig, logger *RuntimeLogger, weCom WeComStatusProvider, connections ConnectionSnapshotProvider, sessions SessionSnapshotProvider, credentials CredentialSnapshotProvider) (*RuntimeInspector, error) {
	if logger == nil || logger.Logger == nil || weCom == nil || connections == nil || sessions == nil || credentials == nil {
		return nil, errInvalidRuntimeDependency
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.StartedAt.IsZero() {
		config.StartedAt = config.Now()
	}
	if config.PID <= 0 {
		config.PID = os.Getpid()
	}
	if config.GOOS == "" {
		config.GOOS = goruntime.GOOS
	}
	if config.GOARCH == "" {
		config.GOARCH = goruntime.GOARCH
	}
	if config.Version == "" {
		config.Version = version.Version
	}
	if config.Commit == "" {
		config.Commit = version.Commit
	}
	if config.BuiltAt == "" {
		config.BuiltAt = version.BuiltAt
	}
	return &RuntimeInspector{
		config: config, logger: logger, weCom: weCom, connections: connections,
		sessions: sessions, credentials: credentials,
	}, nil
}

// Status 返回同一观测时间点聚合的安全运行状态。
func (inspector *RuntimeInspector) Status() RuntimeStatus {
	if inspector == nil {
		return RuntimeStatus{}
	}
	observedAt := inspector.config.Now()
	connections := inspector.connections.Connections()
	sessions := inspector.sessions.ManagementSessions(server.SessionFilter{})
	principals := make(map[string]struct{}, len(connections))
	for _, connection := range connections {
		if connection.PrincipalID != "" {
			principals[connection.PrincipalID] = struct{}{}
		}
	}
	uptime := observedAt.Sub(inspector.config.StartedAt)
	if uptime < 0 {
		uptime = 0
	}
	return RuntimeStatus{
		ObservedAt: observedAt, StartedAt: inspector.config.StartedAt, Uptime: uptime,
		Version: inspector.config.Version, Commit: inspector.config.Commit, BuiltAt: inspector.config.BuiltAt,
		PID: inspector.config.PID, GOOS: inspector.config.GOOS, GOARCH: inspector.config.GOARCH,
		HPAP: adminproto.Protocol, HPRP: hprp.ProtocolVersion,
		RelayListen: inspector.config.RelayListen, AdminSocket: inspector.config.AdminSocket,
		TLS: inspector.config.TLS, WeCom: inspector.weCom.Status(),
		DebugEnabled: inspector.logger.DebugEnabled(), BaseLogLevel: levelName(inspector.logger.BaseLevel()),
		PrincipalCount: len(principals), ConnectionCount: len(connections), SessionCount: len(sessions),
		Credentials: countCredentials(inspector.credentials.List(), observedAt),
	}
}

// EnableDebug 立即开启当前进程的 debug 日志。
func (inspector *RuntimeInspector) EnableDebug() {
	if inspector != nil {
		inspector.logger.EnableDebug()
	}
}

// DisableDebug 立即恢复当前进程的配置基础日志级别。
func (inspector *RuntimeInspector) DisableDebug() {
	if inspector != nil {
		inspector.logger.DisableDebug()
	}
}

// RequestStop 至多调用一次服务根停止回调，并报告本次是否首次触发。
func (inspector *RuntimeInspector) RequestStop() bool {
	if inspector == nil {
		return false
	}
	triggered := false
	inspector.stopOnce.Do(func() {
		triggered = true
		if inspector.logger.Logger != nil {
			inspector.logger.Logger.Info("服务端停止已请求", "source", "admin")
		}
		if inspector.config.Stop != nil {
			inspector.config.Stop()
		}
	})
	return triggered
}

func countCredentials(records []credential.Record, now time.Time) CredentialCounts {
	var counts CredentialCounts
	for _, record := range records {
		if record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
			counts.Expired++
			continue
		}
		switch record.Status {
		case credential.StatusEnabled:
			counts.Enabled++
		case credential.StatusDisabled:
			counts.Disabled++
		}
	}
	return counts
}

func levelName(level slog.Level) string {
	return strings.ToLower(level.String())
}
