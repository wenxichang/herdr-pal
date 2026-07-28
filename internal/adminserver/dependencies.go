package adminserver

import (
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/server"
)

// CredentialManager 提供 Key 生命周期和来源策略的持久化管理能力。
type CredentialManager interface {
	Issue(principalID, machineID string, allowedSources []string, expiresAt *time.Time) (string, credential.Record, error)
	List() []credential.Record
	Show(credentialID uint64) (credential.Record, error)
	Enable(credentialID uint64) (credential.Record, error)
	Disable(credentialID uint64) (credential.Record, error)
	Delete(credentialID uint64) (credential.Record, error)
	AddSources(credentialID uint64, values []string) (credential.Record, error)
	RemoveSources(credentialID uint64, values []string) (credential.Record, error)
	SetSources(credentialID uint64, values []string) (credential.Record, error)
}

// ConnectionManager 提供 HPRP 连接查询、显式断开和凭据策略联动撤下能力。
type ConnectionManager interface {
	Connections() []server.ConnectionView
	Connection(connectionID string) (server.ConnectionView, bool)
	DisconnectConnection(connectionID, reason string) bool
	DisconnectCredential(credentialID uint64, reason string) int
	RevalidateCredentialSource(credentialID uint64, rules []credential.SourceRule, reason string) int
}

// SessionInspector 提供不读取终端且不改变用户路由状态的会话快照。
type SessionInspector interface {
	ManagementSessions(server.SessionFilter) []server.SessionView
}

// RuntimeInspector 提供安全状态快照和不持久化到配置文件的运行时控制能力。
type RuntimeInspector interface {
	Status() adminproto.ServerStatusResult
	EnableDebug()
	DisableDebug()
	RequestStop() bool
}
