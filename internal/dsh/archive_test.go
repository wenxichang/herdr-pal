package dsh

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

// archiveSessionItems 构造两个可控制会话，用于验证归档过滤。
func archiveSessionItems() []map[string]any {
	return []map[string]any{
		{"sessionId": "keep", "agentAvailable": true, "updatedAt": 3,
			"projections": map[string]any{"values": map[string]any{"title": "保留"}}},
		{"sessionId": "archived", "agentAvailable": true, "updatedAt": 2,
			"projections": map[string]any{"values": map[string]any{"title": "已归档"}}},
	}
}

// applyWorkspaceFrame 把一帧 workspace 流的原始 JSON 交给 Mux 处理。
func applyWorkspaceFrame(t *testing.T, mux *Mux, raw string) {
	t.Helper()
	if err := mux.handleFrame([]byte(raw)); err != nil {
		t.Fatalf("handleFrame(%s) 返回错误：%v", raw, err)
	}
}

// sessionIDs 返回目录中的会话 ID。
func archiveTestSessionIDs(sessions []SessionSummary) []string {
	ids := make([]string, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	return ids
}

// TestSessionsFiltersArchivedSessions 验证 baseline 中的归档会话被排除。
//
// 归档会话仍持有 Agent 控制权（agentAvailable=true）并出现在 session/list 中，
// 只能靠 workspace/follow 的归档集合识别，这是与冷历史完全不同的过滤条件。
func TestSessionsFiltersArchivedSessions(t *testing.T) {
	client, _ := newSessionsTestClient(t, archiveSessionItems())

	// 工作区流尚未就绪时无法判定，不能误删目录。
	sessions, err := client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("读取会话目录: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("未就绪时应保留全部会话，实际 %v", archiveTestSessionIDs(sessions))
	}
	if _, seen := client.ArchivedSessions(context.Background()); seen {
		t.Fatal("未收到 baseline 前归档集合不应视为可用")
	}

	applyWorkspaceFrame(t, client.mux,
		`{"streamId":"workspace","type":"item","value":{"type":"baseline","value":{"archivedSessionIds":["archived"]}}}`)

	archived, seen := client.ArchivedSessions(context.Background())
	if !seen || len(archived) != 1 {
		t.Fatalf("归档集合 = %v seen=%v", archived, seen)
	}
	if _, ok := archived["archived"]; !ok {
		t.Fatalf("归档集合缺少目标会话：%v", archived)
	}

	sessions, err = client.Sessions(context.Background())
	if err != nil {
		t.Fatalf("读取会话目录: %v", err)
	}
	if got := archiveTestSessionIDs(sessions); len(got) != 1 || got[0] != "keep" {
		t.Fatalf("过滤后 = %v，期望只有 keep", got)
	}
}

// TestMuxAppliesIncrementalArchivedFrame 验证增量归档帧使用顶层字段布局。
func TestMuxAppliesIncrementalArchivedFrame(t *testing.T) {
	mux := newMux(testValidHost(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// baseline 先给出一个归档会话。
	applyWorkspaceFrame(t, mux,
		`{"streamId":"workspace","type":"item","value":{"type":"baseline","value":{"archivedSessionIds":["a"]}}}`)
	archived, _ := mux.archivedSnapshot()
	if _, ok := archived["a"]; !ok {
		t.Fatalf("baseline 未生效：%v", archived)
	}

	// 增量帧的 archivedSessionIds 在顶层，且是完整集合而不是差量。
	applyWorkspaceFrame(t, mux,
		`{"streamId":"workspace","type":"item","value":{"type":"archived","archivedSessionIds":["b","c"]}}`)
	archived, seen := mux.archivedSnapshot()
	if !seen || len(archived) != 2 {
		t.Fatalf("增量帧后归档集合 = %v seen=%v", archived, seen)
	}
	if _, ok := archived["b"]; !ok {
		t.Errorf("增量帧应包含 b：%v", archived)
	}
	if _, ok := archived["a"]; ok {
		t.Errorf("整体替换后不应保留 a：%v", archived)
	}
}

// TestMuxArchivedFrameToEmptyCollectionClears 验证空数组清空归档集合。
//
// 取消归档时 dsh 推送 archivedSessionIds=[]，必须与“没有该字段”区分开：
// 前者表示清空，后者表示无法判定。
func TestMuxArchivedFrameToEmptyCollectionClears(t *testing.T) {
	mux := newMux(testValidHost(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	applyWorkspaceFrame(t, mux,
		`{"streamId":"workspace","type":"item","value":{"type":"archived","archivedSessionIds":["a"]}}`)
	if archived, _ := mux.archivedSnapshot(); len(archived) != 1 {
		t.Fatalf("归档集合 = %v", archived)
	}

	applyWorkspaceFrame(t, mux,
		`{"streamId":"workspace","type":"item","value":{"type":"archived","archivedSessionIds":[]}}`)
	archived, seen := mux.archivedSnapshot()
	if !seen {
		t.Fatal("空集合仍应视为已就绪")
	}
	if len(archived) != 0 {
		t.Fatalf("归档集合应清空，实际 %v", archived)
	}
}

// TestMuxIgnoresUnknownWorkspaceFrames 验证未知帧不影响 $events 订阅。
func TestMuxIgnoresUnknownWorkspaceFrames(t *testing.T) {
	mux := newMux(testValidHost(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, raw := range []string{
		`{"streamId":"workspace","type":"item","value":{"type":"pinned","pinnedSessionIds":["x"]}}`,
		`{"streamId":"workspace","type":"item","value":"not-an-object"}`,
		`{"streamId":"workspace","type":"other","value":{}}`,
	} {
		applyWorkspaceFrame(t, mux, raw)
	}
	if _, seen := mux.archivedSnapshot(); seen {
		t.Fatal("未知帧不应把归档集合标记为可用")
	}

	// $events 流仍可正常处理 ready。
	if err := mux.handleFrame([]byte(`{"streamId":"events","type":"item","value":{"type":"ready","clientId":"c1"}}`)); err != nil {
		t.Fatalf("ready 帧失败：%v", err)
	}
	if !mux.isReady() {
		t.Fatal("ready 帧未生效")
	}
}

// TestMuxBoundsArchivedCollection 验证超长归档集合被忽略而不影响运行。
func TestMuxBoundsArchivedCollection(t *testing.T) {
	mux := newMux(testValidHost(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ids := make([]string, muxMaxArchivedIDs+1)
	for index := range ids {
		ids[index] = "session-x"
	}
	raw, err := json.Marshal(map[string]any{
		"streamId": "workspace", "type": "item",
		"value": map[string]any{"type": "archived", "archivedSessionIds": ids},
	})
	if err != nil {
		t.Fatalf("构造帧失败：%v", err)
	}
	applyWorkspaceFrame(t, mux, string(raw))
	if _, seen := mux.archivedSnapshot(); seen {
		t.Fatal("超长集合应被忽略")
	}
}

// TestClientArchivedSessionsWithoutMux 验证缺少 Mux 时安全返回不可用。
func TestClientArchivedSessionsWithoutMux(t *testing.T) {
	var nilClient *Client
	if _, seen := nilClient.ArchivedSessions(context.Background()); seen {
		t.Fatal("nil 客户端应返回不可用")
	}
	client := &Client{}
	if _, seen := client.ArchivedSessions(context.Background()); seen {
		t.Fatal("缺少 Mux 时应返回不可用")
	}
}
