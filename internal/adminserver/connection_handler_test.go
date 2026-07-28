package adminserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestConnectionHandlerListShowDisconnectAndPagination(t *testing.T) {
	now := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	manager := &fakeConnectionManager{views: []server.ConnectionView{
		managementConnectionView("c-1", 1, "user-a", "home"),
		managementConnectionView("c-2", 2, "user-a", "office"),
		managementConnectionView("c-3", 3, "user-b", "home"),
	}}
	handler, err := NewConnectionHandler(manager, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first := handleConnectionRequest(t, handler, adminproto.MethodConnectionList, adminproto.ConnectionListParams{Limit: 2})
	var firstPage adminproto.ConnectionListResult
	decodeKeyResult(t, first.Response, &firstPage)
	if firstPage.ObservedAt != now || len(firstPage.Items) != 2 || firstPage.Items[0].ConnectionID != "c-1" || firstPage.NextPageToken == "" {
		t.Fatalf("first connection page = %#v", firstPage)
	}
	second := handleConnectionRequest(t, handler, adminproto.MethodConnectionList, adminproto.ConnectionListParams{Limit: 2, PageToken: firstPage.NextPageToken})
	var secondPage adminproto.ConnectionListResult
	decodeKeyResult(t, second.Response, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].ConnectionID != "c-3" || secondPage.NextPageToken != "" {
		t.Fatalf("second connection page = %#v", secondPage)
	}

	show := handleConnectionRequest(t, handler, adminproto.MethodConnectionShow, adminproto.ConnectionIDParams{ConnectionID: "c-2"})
	var shown adminproto.ConnectionResult
	decodeKeyResult(t, show.Response, &shown)
	if shown.Connection.ConnectionID != "c-2" || shown.Connection.Implementation.Version != "v1.2.3" || shown.Connection.SourceIP != "192.168.1.10" {
		t.Fatalf("connection show = %#v", shown)
	}
	missing := handleConnectionRequest(t, handler, adminproto.MethodConnectionShow, adminproto.ConnectionIDParams{ConnectionID: "missing"})
	assertKeyError(t, missing.Response, adminproto.CodeConnectionNotFound)

	disconnected := handleConnectionRequest(t, handler, adminproto.MethodConnectionDisconnect, adminproto.ConnectionIDParams{ConnectionID: "c-2"})
	var disconnectResult adminproto.ConnectionDisconnectResult
	decodeKeyResult(t, disconnected.Response, &disconnectResult)
	if !disconnectResult.Disconnected || disconnectResult.ConnectionID != "c-2" || len(manager.disconnected) != 1 {
		t.Fatalf("disconnect result=%#v calls=%v", disconnectResult, manager.disconnected)
	}
	missingDisconnect := handleConnectionRequest(t, handler, adminproto.MethodConnectionDisconnect, adminproto.ConnectionIDParams{ConnectionID: "missing"})
	assertKeyError(t, missingDisconnect.Response, adminproto.CodeConnectionNotFound)
}

func TestConnectionHandlerRejectsInvalidParamsAndPageToken(t *testing.T) {
	handler, err := NewConnectionHandler(&fakeConnectionManager{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: adminproto.MethodConnectionShow, Params: json.RawMessage(`{"connection_id":""}`)})
	if err != nil {
		t.Fatal(err)
	}
	assertKeyError(t, result.Response, adminproto.CodeArgumentInvalid)
	invalidPage := handleConnectionRequest(t, handler, adminproto.MethodConnectionList, adminproto.ConnectionListParams{PageToken: "bad"})
	assertKeyError(t, invalidPage.Response, adminproto.CodeArgumentInvalid)
}

func handleConnectionRequest(t *testing.T, handler *ConnectionHandler, method adminproto.Method, params any) HandleResult {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: method, Params: encoded})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func managementConnectionView(connectionID string, credentialID uint64, principalID, machineID string) server.ConnectionView {
	return server.ConnectionView{
		ConnectionID: connectionID, CredentialID: credentialID, PrincipalID: principalID, MachineID: machineID,
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "v1.2.3", OS: "linux", Arch: "amd64"},
		SourceIP:       "192.168.1.10", ConnectedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
		LastHeartbeatAt: time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC), LastSnapshotAt: time.Date(2026, 7, 28, 12, 2, 0, 0, time.UTC),
		SnapshotSequence: 7, SessionCount: 2, Capabilities: []string{"command.output.v1"}, Ready: true,
	}
}

type fakeConnectionManager struct {
	ConnectionManager
	views        []server.ConnectionView
	disconnected []string
}

func (manager *fakeConnectionManager) Connections() []server.ConnectionView {
	return append([]server.ConnectionView(nil), manager.views...)
}

func (manager *fakeConnectionManager) Connection(connectionID string) (server.ConnectionView, bool) {
	for _, view := range manager.views {
		if view.ConnectionID == connectionID {
			return view, true
		}
	}
	return server.ConnectionView{}, false
}

func (manager *fakeConnectionManager) DisconnectConnection(connectionID, _ string) bool {
	if _, exists := manager.Connection(connectionID); !exists {
		return false
	}
	manager.disconnected = append(manager.disconnected, connectionID)
	return true
}
