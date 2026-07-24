// Package relayclient 实现 herdr-pal 到中央 Relay Server 的 WSS 客户端。
package relayclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// BuildSnapshot 将本地稳定排序目标转换为完整 Relay 会话快照。
func BuildSnapshot(sequence uint64, targets []session.Target) relayproto.SessionSnapshot {
	sessions := make([]relayproto.Session, len(targets))
	for index, target := range targets {
		sessions[index] = relayproto.Session{
			LocalIndex: index + 1, PaneID: target.PaneID, TerminalID: target.TerminalID,
			OccupantHash: target.OccupantKey, Agent: target.Agent, DisplayAgent: target.DisplayAgent,
			Title: target.Title, Workspace: target.Workspace, Tab: target.Tab, Status: string(target.Status),
		}
	}
	return relayproto.SessionSnapshot{Sequence: sequence, Sessions: sessions}
}

// SnapshotFingerprint 返回忽略 sequence 的快照内容摘要。
func SnapshotFingerprint(snapshot relayproto.SessionSnapshot) string {
	encoded, _ := json.Marshal(snapshot.Sessions)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest)
}
