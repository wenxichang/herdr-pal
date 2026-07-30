package adminserver

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestSessionHandlerListsFullDisplayTargetFiltersAndPagination(t *testing.T) {
	now := time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC)
	inspector := &fakeSessionInspector{views: []server.SessionView{
		managementSessionView("user-a", 1, "home", "w1:p1", "session-1", hprp.StatusWorking),
		managementSessionView("user-a", 2, "office", "w2:p2", "session-2", hprp.StatusBlocked),
		managementSessionView("user-b", 1, "home", "w3:p3", "session-3", hprp.StatusDone),
	}}
	handler, err := NewSessionHandler(newAdminServiceForTest(t, nil, nil, inspector, nil, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	first := handleSessionRequest(t, handler, adminproto.SessionListParams{Limit: 2})
	var firstPage adminproto.SessionListResult
	decodeKeyResult(t, first.Response, &firstPage)
	if firstPage.ObservedAt != now || len(firstPage.Items) != 2 || firstPage.NextPageToken == "" {
		t.Fatalf("first session page = %#v", firstPage)
	}
	item := firstPage.Items[0]
	if item.PrincipalID != "user-a" || item.Number != 1 || item.Target != (adminproto.SessionTarget{MachineID: "home", SlotID: "w1:p1", SessionID: "session-1"}) {
		t.Fatalf("session target = %#v", item)
	}
	if item.Workspace != "workspace" || item.Tab != "main" || item.WorkspaceLabel != "workspace/main" || item.Agent != "codex" || item.DisplayAgent != "Codex" || item.Pane != "w1:p1" || item.Title != "title" || item.StatusLabel != "working ⏳" {
		t.Fatalf("session display = %#v", item)
	}
	second := handleSessionRequest(t, handler, adminproto.SessionListParams{Limit: 2, PageToken: firstPage.NextPageToken})
	var secondPage adminproto.SessionListResult
	decodeKeyResult(t, second.Response, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].PrincipalID != "user-b" || secondPage.NextPageToken != "" {
		t.Fatalf("second session page = %#v", secondPage)
	}

	filtered := handleSessionRequest(t, handler, adminproto.SessionListParams{PrincipalID: "user-a", MachineID: "office"})
	var filteredResult adminproto.SessionListResult
	decodeKeyResult(t, filtered.Response, &filteredResult)
	if len(filteredResult.Items) != 1 || filteredResult.Items[0].Number != 2 || inspector.lastFilter != (server.SessionFilter{PrincipalID: "user-a", MachineID: "office"}) {
		t.Fatalf("filtered sessions=%#v filter=%#v", filteredResult, inspector.lastFilter)
	}
}

func TestSessionHandlerRejectsUnknownParamsAndDoesNotNeedOutputReader(t *testing.T) {
	inspector := &fakeSessionInspector{}
	handler, err := NewSessionHandler(newAdminServiceForTest(t, nil, nil, inspector, nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: adminproto.MethodSessionList, Params: json.RawMessage(`{"output":"should-not-be-read"}`)})
	if err != nil {
		t.Fatal(err)
	}
	assertKeyError(t, result.Response, adminproto.CodeArgumentInvalid)
	if inspector.calls != 0 {
		t.Fatalf("invalid params queried sessions %d times", inspector.calls)
	}
}

func TestSessionHandlerDoesNotModifyCatalogNumberingOrSelection(t *testing.T) {
	catalog := server.NewSessionCatalog()
	if _, err := catalog.Attach("connection-1", server.ClientKey{UserID: "user-a", MachineID: "home"}); err != nil {
		t.Fatal(err)
	}
	snapshot := hprp.SessionSnapshot{Sequence: 1, Sessions: []hprp.Session{{
		SlotID: "w1:p1", SessionID: "session-1", Status: hprp.StatusIdle,
		Display: hprp.SessionDisplay{Index: 1, Agent: "codex", DisplayAgent: "Codex", Workspace: "workspace", Tab: "main"},
	}}}
	if err := catalog.ApplySnapshot("connection-1", snapshot); err != nil {
		t.Fatal(err)
	}
	target := hprp.Target{MachineID: "home", SlotID: "w1:p1", SessionID: "session-1"}
	if err := catalog.SetSelection("user-a", target); err != nil {
		t.Fatal(err)
	}
	handler, err := NewSessionHandler(newAdminServiceForTest(t, nil, nil, catalog, nil, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	response := handleSessionRequest(t, handler, adminproto.SessionListParams{PrincipalID: "user-a"})
	var result adminproto.SessionListResult
	decodeKeyResult(t, response.Response, &result)
	if len(result.Items) != 1 {
		t.Fatalf("session result = %#v", result)
	}
	if _, err := catalog.ResolveNumbered("user-a", 1); !errors.Is(err, server.ErrNoListSnapshot) {
		t.Fatalf("session handler changed /ls numbering: %v", err)
	}
	selected, err := catalog.Selected("user-a")
	if err != nil || selected.Ref != target {
		t.Fatalf("session handler changed selection: %#v, %v", selected, err)
	}
}

func handleSessionRequest(t *testing.T, handler *SessionHandler, params adminproto.SessionListParams) HandleResult {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: adminproto.MethodSessionList, Params: encoded})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func managementSessionView(principalID string, number int, machineID, slotID, sessionID, status string) server.SessionView {
	return server.SessionView{
		PrincipalID: principalID, Number: number,
		Target: hprp.Target{MachineID: machineID, SlotID: slotID, SessionID: sessionID},
		Session: hprp.Session{SlotID: slotID, SessionID: sessionID, Status: status, Display: hprp.SessionDisplay{
			Index: number, Agent: "codex", DisplayAgent: "Codex", Workspace: "workspace", Tab: "main", Title: "title",
		}},
		WorkspaceLabel: "workspace/main", StatusLabel: map[string]string{
			hprp.StatusWorking: "working ⏳", hprp.StatusBlocked: "blocked ⁉️", hprp.StatusDone: "done ✅",
		}[status],
	}
}

type fakeSessionInspector struct {
	views      []server.SessionView
	lastFilter server.SessionFilter
	calls      int
}

func (inspector *fakeSessionInspector) ManagementSessions(filter server.SessionFilter) []server.SessionView {
	inspector.calls++
	inspector.lastFilter = filter
	result := make([]server.SessionView, 0, len(inspector.views))
	for _, view := range inspector.views {
		if filter.PrincipalID != "" && view.PrincipalID != filter.PrincipalID || filter.MachineID != "" && view.Target.MachineID != filter.MachineID {
			continue
		}
		result = append(result, view)
	}
	return result
}
