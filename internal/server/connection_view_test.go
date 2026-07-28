package server

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

func TestClientConnectionMetadataSnapshotIncludesNegotiatedRuntimeState(t *testing.T) {
	connection := newClientConnection(context.Background(), clientConnectionConfig{
		ID: "connection-1", CredentialID: 12, Key: ClientKey{UserID: "user-a", MachineID: "home-mac"},
		Source:         netip.MustParseAddr("192.168.1.20"),
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "v1.2.3", OS: "linux", Arch: "amd64"},
		SendCapacity:   2, MaxPending: 2,
	})
	connection.setCapabilities([]string{"z.capability", hprp.CapabilityCommandOutputV1})
	connection.ready.Store(true)
	snapshotAt := time.Date(2026, 7, 28, 12, 1, 0, 0, time.UTC)
	heartbeatAt := snapshotAt.Add(time.Minute)
	connection.recordSnapshot(7, 3, snapshotAt)
	connection.recordHeartbeat(heartbeatAt)

	view := connection.view()
	if view.ConnectionID != "connection-1" || view.CredentialID != 12 || view.PrincipalID != "user-a" || view.MachineID != "home-mac" {
		t.Fatalf("identity view = %#v", view)
	}
	if view.Implementation.Name != "herdr-pal" || view.Implementation.Version != "v1.2.3" || view.SourceIP != "192.168.1.20" || !view.Ready {
		t.Fatalf("runtime view = %#v", view)
	}
	if view.ConnectedAt.IsZero() || !view.LastSnapshotAt.Equal(snapshotAt) || !view.LastHeartbeatAt.Equal(heartbeatAt) || view.SnapshotSequence != 7 || view.SessionCount != 3 {
		t.Fatalf("time/snapshot view = %#v", view)
	}
	wantCapabilities := []string{hprp.CapabilityCommandOutputV1, "z.capability"}
	if !reflect.DeepEqual(view.Capabilities, wantCapabilities) {
		t.Fatalf("Capabilities = %#v, want %#v", view.Capabilities, wantCapabilities)
	}
	view.Capabilities[0] = "mutated"
	if connection.view().Capabilities[0] != hprp.CapabilityCommandOutputV1 {
		t.Fatal("view leaked capabilities backing array")
	}
}

func TestClientHubConnectionsSortsAndFindsByConnectionID(t *testing.T) {
	hub := newHPRPTestHub(t)
	connections := []*clientConnection{
		managementConnection("connection-z", 2, "user-b", "z-machine", "10.0.0.2"),
		managementConnection("connection-a", 1, "user-a", "a-machine", "10.0.0.1"),
	}
	for _, connection := range connections {
		hub.install(connection)
	}
	views := hub.Connections()
	if len(views) != 2 || views[0].ConnectionID != "connection-a" || views[1].ConnectionID != "connection-z" {
		t.Fatalf("Connections() = %#v", views)
	}
	view, exists := hub.Connection("connection-z")
	if !exists || view.CredentialID != 2 || view.PrincipalID != "user-b" {
		t.Fatalf("Connection() = %#v, %t", view, exists)
	}
	if _, exists := hub.Connection("missing"); exists {
		t.Fatal("Connection(missing) exists")
	}
}

func TestClientHubDisconnectConnectionWithdrawsRoutingBeforeCancellation(t *testing.T) {
	hub := newHPRPTestHub(t)
	connection := managementConnection("connection-1", 1, "user-a", "home", "192.168.1.20")
	if _, err := hub.catalog.Attach(connection.id, connection.key); err != nil {
		t.Fatal(err)
	}
	hub.install(connection)
	if !hub.DisconnectConnection(connection.id, "admin request") {
		t.Fatal("DisconnectConnection() = false")
	}
	if hub.readyClient(connection.key) != nil || hub.catalog.HasMachine("user-a", "home") {
		t.Fatal("connection remained routable after disconnect")
	}
	select {
	case <-connection.ctx.Done():
	default:
		t.Fatal("connection context was not canceled")
	}
	if hub.DisconnectConnection(connection.id, "duplicate") {
		t.Fatal("duplicate disconnect succeeded")
	}
}

func TestClientHubDisconnectCredentialAndRevalidateSourceTargetOnlyMatchingConnections(t *testing.T) {
	hub := newHPRPTestHub(t)
	first := managementConnection("connection-1", 7, "user-a", "home", "192.168.1.20")
	second := managementConnection("connection-2", 8, "user-b", "office", "10.0.0.20")
	for _, connection := range []*clientConnection{first, second} {
		if _, err := hub.catalog.Attach(connection.id, connection.key); err != nil {
			t.Fatal(err)
		}
		hub.install(connection)
	}
	if count := hub.RevalidateCredentialSource(7, []credential.SourceRule{"10.0.0.0/8"}, "source policy changed"); count != 1 {
		t.Fatalf("RevalidateCredentialSource() = %d, want 1", count)
	}
	if first.ctx.Err() == nil || second.ctx.Err() != nil {
		t.Fatalf("source revalidation canceled first/second = %v/%v", first.ctx.Err(), second.ctx.Err())
	}
	if count := hub.DisconnectCredential(8, "credential disabled"); count != 1 {
		t.Fatalf("DisconnectCredential() = %d, want 1", count)
	}
	if second.ctx.Err() == nil || len(hub.Connections()) != 0 {
		t.Fatalf("credential disconnect state = %v, %#v", second.ctx.Err(), hub.Connections())
	}
}

func managementConnection(connectionID string, credentialID uint64, principalID, machineID, source string) *clientConnection {
	connection := newClientConnection(context.Background(), clientConnectionConfig{
		ID: connectionID, CredentialID: credentialID, Key: ClientKey{UserID: principalID, MachineID: machineID},
		Source:         netip.MustParseAddr(source),
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "test", OS: "linux", Arch: "amd64"},
		SendCapacity:   2, MaxPending: 2,
	})
	connection.ready.Store(true)
	return connection
}
