// Package relayclient 实现 herdr-pal 到中央 Relay Server 的 WSS 客户端。
package relayclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// BuildSnapshot 将本地稳定排序目标转换为完整 HPRP 会话快照。
//
// 标题和工作区在此再次按 UTF-8 边界截断到 HPRP 标签上限，作为执行器之外的
// 第二道防线：超限标签会让 Server 拒绝整条快照并导致连接重连循环。
func BuildSnapshot(sequence uint64, targets []session.Target) hprp.SessionSnapshot {
	sessions := make([]hprp.Session, len(targets))
	for index, target := range targets {
		sessions[index] = hprp.Session{
			SlotID: target.PaneID, SessionID: target.OccupantKey,
			Display: hprp.SessionDisplay{
				Index: index + 1, Agent: target.Agent, DisplayAgent: target.DisplayAgent,
				Title: truncateHPRPLabel(target.Title), Workspace: truncateHPRPLabel(target.Workspace), Tab: target.Tab,
			},
			Status: string(target.Status),
		}
	}
	return hprp.SessionSnapshot{Sequence: sequence, Sessions: sessions}
}

// truncateHPRPLabel 按 UTF-8 边界截断 HPRP 展示标签。
func truncateHPRPLabel(value string) string {
	if len(value) <= hprp.MaxLabelBytes {
		return value
	}
	cut := hprp.MaxLabelBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// SnapshotFingerprint 返回忽略 sequence 的快照内容摘要。
func SnapshotFingerprint(snapshot hprp.SessionSnapshot) string {
	encoded, _ := json.Marshal(snapshot.Sessions)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest)
}
