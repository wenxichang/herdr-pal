// Package notificationprefs 保存 Server 按业务组合匹配的会话通知偏好。
package notificationprefs

import (
	"errors"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

// ErrInvalidPreference 表示通知身份或持久化格式无效。
var ErrInvalidPreference = errors.New("通知偏好无效")

// Identity 是通知开关共享的业务身份，不依赖编号或临时会话 ID。
type Identity struct {
	PrincipalID string `json:"principal_id"`
	MachineID   string `json:"machine_id"`
	Title       string `json:"title"`
	Workspace   string `json:"workspace"`
	Tab         string `json:"tab"`
	CLIType     string `json:"cli_type"`
}

// NewIdentity 从已认证身份和目录元数据生成可比较的通知组合。
func NewIdentity(principalID, machineID string, display hprp.SessionDisplay) (Identity, error) {
	if credential.ValidatePrincipalID(principalID) != nil || hprp.ValidateMachineID(machineID) != nil {
		return Identity{}, ErrInvalidPreference
	}
	cli := strings.TrimSpace(display.Agent)
	if cli == "" {
		cli = strings.TrimSpace(display.DisplayAgent)
	}
	if cli == "" {
		cli = "unknown"
	}
	return Identity{PrincipalID: principalID, MachineID: machineID, Title: strings.TrimSpace(display.Title), Workspace: strings.TrimSpace(display.Workspace), Tab: strings.TrimSpace(display.Tab), CLIType: strings.ToLower(cli)}, nil
}

func validIdentity(key Identity) bool {
	normalized, err := NewIdentity(key.PrincipalID, key.MachineID, hprp.SessionDisplay{Title: key.Title, Workspace: key.Workspace, Tab: key.Tab, Agent: key.CLIType})
	return err == nil && normalized == key
}
