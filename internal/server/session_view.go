package server

import (
	"sort"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/panel"
)

// SessionFilter 限定管理面要观察的 principal 和机器；空字段表示不过滤。
type SessionFilter struct {
	PrincipalID string
	MachineID   string
}

// SessionView 是不改变用户路由状态的在线 Agent 会话快照。
type SessionView struct {
	PrincipalID    string
	Number         int
	Target         hprp.Target
	Session        hprp.Session
	WorkspaceLabel string
	StatusLabel    string
}

// ManagementSessions 返回按 principal 和 `/ls` 规则稳定排序的会话快照。
//
// Number 始终是会话在该 principal 完整当前列表中的编号；过滤不会重排编号，也不会创建或
// 修改企业微信 `/ls` 的编号缓存和当前选择。
func (catalog *SessionCatalog) ManagementSessions(filter SessionFilter) []SessionView {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	principals := make(map[string]struct{})
	for key, machine := range catalog.machines {
		if machine.sequence == 0 || filter.PrincipalID != "" && key.UserID != filter.PrincipalID {
			continue
		}
		principals[key.UserID] = struct{}{}
	}
	principalIDs := make([]string, 0, len(principals))
	for principalID := range principals {
		principalIDs = append(principalIDs, principalID)
	}
	sort.Strings(principalIDs)
	views := make([]SessionView, 0)
	for _, principalID := range principalIDs {
		entries := catalog.entriesLocked(principalID)
		for index, entry := range entries {
			if filter.MachineID != "" && entry.Ref.MachineID != filter.MachineID {
				continue
			}
			session := entry.Session
			session.Status = hprp.NormalizeStatus(session.Status)
			views = append(views, SessionView{
				PrincipalID:    principalID,
				Number:         index + 1,
				Target:         entry.Ref,
				Session:        session,
				WorkspaceLabel: panel.WorkspaceLabel(session.Display.Workspace, session.Display.Tab),
				StatusLabel:    panel.AgentStatusLabelValue(session.Status),
			})
		}
	}
	return views
}
