package adminservice

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestServiceDisablesCredentialBeforeDisconnect(t *testing.T) {
	store, record := seededServiceStore(t, "home", "127.0.0.1")
	disabledBeforeDisconnect := false
	connections := &fakeConnections{onDisconnectCredential: func(credentialID uint64) {
		shown, err := store.Show(credentialID)
		disabledBeforeDisconnect = err == nil && shown.Status == credential.StatusDisabled
	}}
	service := newTestService(t, store, connections, &fakeRuntime{})

	result, err := service.SetCredentialEnabled(record.CredentialID, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Credential.Status != string(credential.StatusDisabled) || result.DisconnectedConnections != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !disabledBeforeDisconnect {
		t.Fatal("credential was disconnected before disabled state persisted")
	}
}

func TestServiceDoesNotDisconnectWhenCredentialPersistenceFails(t *testing.T) {
	store, record := seededServiceStore(t, "home", "127.0.0.1")
	failing := &failingCredentialManager{CredentialManager: store, disableErr: errors.New("disk unavailable")}
	connections := &fakeConnections{}
	service := newTestService(t, failing, connections, &fakeRuntime{})

	_, err := service.SetCredentialEnabled(record.CredentialID, false)
	if ErrorCodeOf(err) != CodeInternal {
		t.Fatalf("error = %v, code = %s", err, ErrorCodeOf(err))
	}
	if connections.disconnectCredentialCalls != 0 {
		t.Fatalf("disconnect calls = %d", connections.disconnectCredentialCalls)
	}
}

func TestServiceRevalidatesOnlyRestrictiveSourceChanges(t *testing.T) {
	store, record := seededServiceStore(t, "home", "127.0.0.1", "127.0.0.2")
	connections := &fakeConnections{}
	service := newTestService(t, store, connections, &fakeRuntime{})

	if _, err := service.MutateSources(record.CredentialID, SourceAdd, []string{"10.0.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	if connections.revalidateCalls != 0 {
		t.Fatalf("source add revalidated %d times", connections.revalidateCalls)
	}
	result, err := service.MutateSources(record.CredentialID, SourceSet, []string{"192.168.1.1-192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	if connections.revalidateCalls != 1 || result.DisconnectedConnections != 1 {
		t.Fatalf("result=%#v revalidations=%d", result, connections.revalidateCalls)
	}
}

func TestServiceReturnsSafeCredentialViews(t *testing.T) {
	store, record := seededServiceStore(t, "home", "127.0.0.1")
	service := newTestService(t, store, &fakeConnections{}, &fakeRuntime{})

	view, err := service.ShowCredential(record.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if view.CredentialID != record.CredentialID || view.PrincipalID != record.PrincipalID || view.MachineID != record.MachineID {
		t.Fatalf("view = %#v", view)
	}
	if len(view.AllowedSources) != 1 || view.AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("sources = %#v", view.AllowedSources)
	}
}

func TestServiceMapsCredentialValidationAndExhaustionErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{name: "invalid", err: credential.ErrInvalidRecord, want: CodeInvalidArgument},
		{name: "exhausted", err: credential.ErrCredentialIDExhausted, want: CodeServerBusy},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &issueFailingCredentialManager{err: test.err}
			service := newTestService(t, manager, &fakeConnections{}, &fakeRuntime{})
			_, err := service.IssueCredential(IssueCredentialInput{
				PrincipalID: "user-a", MachineID: "home", Sources: []string{"127.0.0.1"},
			})
			if ErrorCodeOf(err) != test.want {
				t.Fatalf("error = %v, code = %s, want %s", err, ErrorCodeOf(err), test.want)
			}
		})
	}
}

func TestServiceListSourcesValidatesCredentialID(t *testing.T) {
	service := newTestService(t, emptyCredentialManager{}, &fakeConnections{}, &fakeRuntime{})
	if _, err := service.ListSources(0); ErrorCodeOf(err) != CodeInvalidArgument {
		t.Fatalf("error = %v, code = %s", err, ErrorCodeOf(err))
	}
}

func TestServicePrepareStopCommitsOnlyAfterResponse(t *testing.T) {
	runtime := &fakeRuntime{}
	service := newTestService(t, emptyCredentialManager{}, &fakeConnections{}, runtime)

	action, err := service.PrepareStop()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.stopCalls != 0 {
		t.Fatal("PrepareStop triggered stop before commit")
	}
	if _, err := service.PrepareStop(); ErrorCodeOf(err) != CodeServerBusy {
		t.Fatalf("second PrepareStop error = %v", err)
	}
	action.Commit()
	action.Commit()
	if runtime.stopCalls != 1 {
		t.Fatalf("stop calls = %d", runtime.stopCalls)
	}
}

func TestServicePrepareStopRollbackAllowsRetry(t *testing.T) {
	service := newTestService(t, emptyCredentialManager{}, &fakeConnections{}, &fakeRuntime{})
	action, err := service.PrepareStop()
	if err != nil {
		t.Fatal(err)
	}
	action.Rollback()
	if _, err := service.PrepareStop(); err != nil {
		t.Fatalf("PrepareStop after rollback error = %v", err)
	}
}

func newTestService(t *testing.T, credentials CredentialManager, connections ConnectionManager, runtime RuntimeController) *Service {
	t.Helper()
	service, err := New(Config{
		Credentials: credentials,
		Connections: connections,
		Sessions:    fakeSessions{},
		Runtime:     runtime,
		Now:         func() time.Time { return time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func seededServiceStore(t *testing.T, machineID string, sources ...string) (*credential.Store, credential.Record) {
	t.Helper()
	store, err := credential.LoadStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, record, err := store.Issue("user-a", machineID, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, record
}

type fakeConnections struct {
	onDisconnectCredential    func(uint64)
	disconnectCredentialCalls int
	revalidateCalls           int
}

func (connections *fakeConnections) Connections() []server.ConnectionView { return nil }
func (connections *fakeConnections) Connection(string) (server.ConnectionView, bool) {
	return server.ConnectionView{}, false
}
func (connections *fakeConnections) DisconnectConnection(string, string) bool { return false }
func (connections *fakeConnections) DisconnectCredential(credentialID uint64, _ string) int {
	connections.disconnectCredentialCalls++
	if connections.onDisconnectCredential != nil {
		connections.onDisconnectCredential(credentialID)
	}
	return 1
}
func (connections *fakeConnections) RevalidateCredentialSource(uint64, []credential.SourceRule, string) int {
	connections.revalidateCalls++
	return 1
}

type fakeSessions struct{}

func (fakeSessions) ManagementSessions(server.SessionFilter) []server.SessionView { return nil }

type fakeRuntime struct {
	status    ServerStatus
	stopCalls int
}

func (runtime *fakeRuntime) Status() ServerStatus { return runtime.status }
func (runtime *fakeRuntime) EnableDebug()         { runtime.status.DebugEnabled = true }
func (runtime *fakeRuntime) DisableDebug()        { runtime.status.DebugEnabled = false }
func (runtime *fakeRuntime) RequestStop() bool {
	runtime.stopCalls++
	return runtime.stopCalls == 1
}

type failingCredentialManager struct {
	CredentialManager
	disableErr error
}

func (manager *failingCredentialManager) Disable(uint64) (credential.Record, error) {
	return credential.Record{}, manager.disableErr
}

type issueFailingCredentialManager struct {
	emptyCredentialManager
	err error
}

func (manager *issueFailingCredentialManager) Issue(string, string, []string, *time.Time) (string, credential.Record, error) {
	return "", credential.Record{}, manager.err
}

type emptyCredentialManager struct{}

func (emptyCredentialManager) Issue(string, string, []string, *time.Time) (string, credential.Record, error) {
	return "", credential.Record{}, errors.New("unused")
}
func (emptyCredentialManager) List() []credential.Record { return nil }
func (emptyCredentialManager) Show(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) Enable(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) Disable(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) Delete(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) AddSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) RemoveSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyCredentialManager) SetSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
