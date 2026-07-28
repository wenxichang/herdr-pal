package server

import (
	"sort"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

// ConnectionView 是一条 HPRP Pal 连接的线程安全运行快照。
type ConnectionView struct {
	ConnectionID     string
	CredentialID     uint64
	PrincipalID      string
	MachineID        string
	Implementation   hprp.Implementation
	SourceIP         string
	ConnectedAt      time.Time
	LastHeartbeatAt  time.Time
	LastSnapshotAt   time.Time
	SnapshotSequence uint64
	SessionCount     int
	Capabilities     []string
	Ready            bool
}

// Connections 返回当前连接的稳定排序快照。
func (hub *ClientHub) Connections() []ConnectionView {
	if hub == nil {
		return nil
	}
	hub.mu.RLock()
	connections := make([]*clientConnection, 0, len(hub.clients))
	for _, connection := range hub.clients {
		connections = append(connections, connection)
	}
	hub.mu.RUnlock()
	views := make([]ConnectionView, 0, len(connections))
	for _, connection := range connections {
		views = append(views, connection.view())
	}
	sort.Slice(views, func(left, right int) bool {
		if views[left].PrincipalID != views[right].PrincipalID {
			return views[left].PrincipalID < views[right].PrincipalID
		}
		if views[left].MachineID != views[right].MachineID {
			return views[left].MachineID < views[right].MachineID
		}
		return views[left].ConnectionID < views[right].ConnectionID
	})
	return views
}

// Connection 按 connection ID 返回当前连接快照。
func (hub *ClientHub) Connection(connectionID string) (ConnectionView, bool) {
	if hub == nil || strings.TrimSpace(connectionID) == "" {
		return ConnectionView{}, false
	}
	hub.mu.RLock()
	defer hub.mu.RUnlock()
	for _, connection := range hub.clients {
		if connection.id == connectionID {
			return connection.view(), true
		}
	}
	return ConnectionView{}, false
}

// DisconnectConnection 撤下并取消指定 HPRP 连接。
func (hub *ClientHub) DisconnectConnection(connectionID, reason string) bool {
	disconnected := hub.withdrawConnections(func(connection *clientConnection) bool {
		return connection.id == connectionID
	}, reason)
	return disconnected == 1
}

// DisconnectCredential 撤下并取消使用指定 credential ID 的全部 HPRP 连接。
func (hub *ClientHub) DisconnectCredential(credentialID uint64, reason string) int {
	return hub.withdrawConnections(func(connection *clientConnection) bool {
		return connection.credentialID == credentialID
	}, reason)
}

// RevalidateCredentialSource 撤下来源地址已不符合新规则的指定凭据连接。
func (hub *ClientHub) RevalidateCredentialSource(credentialID uint64, rules []credential.SourceRule, reason string) int {
	return hub.withdrawConnections(func(connection *clientConnection) bool {
		return connection.credentialID == credentialID && !credential.MatchSource(rules, connection.source)
	}, reason)
}

func (hub *ClientHub) withdrawConnections(match func(*clientConnection) bool, reason string) int {
	if hub == nil || match == nil {
		return 0
	}
	hub.mu.Lock()
	connections := make([]*clientConnection, 0)
	for key, connection := range hub.clients {
		if match(connection) {
			delete(hub.clients, key)
			connections = append(connections, connection)
		}
	}
	hub.mu.Unlock()
	for _, connection := range connections {
		hub.catalog.Detach(connection.id)
		connection.cancel()
		if hub.logger != nil {
			hub.logger.Info("HPRP 客户端被管理面撤下", "credential_id", connection.credentialID, "user_hash", routerHash(connection.key.UserID), "machine_id", safeLogValue(connection.key.MachineID), "connection_id", connection.id, "reason", safeManagementReason(reason))
		}
	}
	return len(connections)
}

func safeManagementReason(reason string) string {
	reason = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(reason, "�"))
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "管理操作请求断开连接"
	}
	if len(reason) > serverLogReasonLimit {
		return reason[:serverLogReasonLimit] + "…"
	}
	return reason
}
