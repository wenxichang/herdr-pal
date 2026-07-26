package panel

import "github.com/wenxichang/herdr-pal/internal/herdr"

// AgentStatusLabel 返回适合列表展示的 Agent 状态和提示图标。
func AgentStatusLabel(status herdr.AgentStatus) string {
	emoji := ""
	switch status {
	case herdr.AgentStatusDone:
		emoji = "✅"
	case herdr.AgentStatusWorking:
		emoji = "⏳"
	case herdr.AgentStatusBlocked:
		emoji = "⁉️"
	case herdr.AgentStatusIdle:
		emoji = "💤"
	case herdr.AgentStatusUnknown:
		emoji = "❔"
	}
	if emoji == "" {
		return string(status)
	}
	return string(status) + " " + emoji
}
