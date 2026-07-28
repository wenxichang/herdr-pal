package panel

import "github.com/wenxichang/herdr-pal/internal/herdr"

// AgentStatusLabel 返回适合列表展示的 Agent 状态和提示图标。
func AgentStatusLabel(status herdr.AgentStatus) string {
	return AgentStatusLabelValue(string(status))
}

// AgentStatusLabelValue 返回适合列表展示的字符串状态和提示图标。
func AgentStatusLabelValue(status string) string {
	emoji := ""
	switch status {
	case string(herdr.AgentStatusDone):
		emoji = "✅"
	case string(herdr.AgentStatusWorking):
		emoji = "⏳"
	case string(herdr.AgentStatusBlocked):
		emoji = "⁉️"
	case string(herdr.AgentStatusIdle):
		emoji = "💤"
	case string(herdr.AgentStatusUnknown):
		emoji = "❔"
	}
	if emoji == "" {
		return status
	}
	return status + " " + emoji
}
