package adminserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func newAdminServiceForTest(t *testing.T, credentials CredentialManager, connections ConnectionManager, sessions SessionInspector, runtime RuntimeInspector, now func() time.Time) *adminservice.Service {
	t.Helper()
	if credentials == nil {
		credentials = emptyAdminCredentialManager{}
	}
	if connections == nil {
		connections = emptyAdminConnectionManager{}
	}
	if sessions == nil {
		sessions = emptyAdminSessionInspector{}
	}
	if runtime == nil {
		runtime = &emptyAdminRuntimeInspector{}
	}
	service, err := adminservice.New(adminservice.Config{
		Credentials:       credentials,
		Connections:       connections,
		Sessions:          sessions,
		Runtime:           runtime,
		Registrations:     emptyAdminRegistrationManager{},
		KeyDelivery:       func(context.Context, machinereg.KeyDelivery) error { return nil },
		RejectionDelivery: func(context.Context, machinereg.RejectionDelivery) error { return nil },
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type emptyAdminRegistrationManager struct{}

func (emptyAdminRegistrationManager) ListPending() []machinereg.Request { return nil }
func (emptyAdminRegistrationManager) Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error) {
	return machinereg.ApprovalResult{}, machinereg.ErrRequestNotFound
}
func (emptyAdminRegistrationManager) Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error) {
	return machinereg.RejectionResult{}, machinereg.ErrRequestNotFound
}

type emptyAdminCredentialManager struct{}

func (emptyAdminCredentialManager) Issue(string, string, []string, *time.Time) (string, credential.Record, error) {
	return "", credential.Record{}, errors.New("unused")
}
func (emptyAdminCredentialManager) List() []credential.Record { return nil }
func (emptyAdminCredentialManager) Show(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) Enable(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) Disable(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) Delete(uint64) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) AddSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) RemoveSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}
func (emptyAdminCredentialManager) SetSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, credential.ErrCredentialNotFound
}

type emptyAdminConnectionManager struct{}

func (emptyAdminConnectionManager) Connections() []server.ConnectionView { return nil }
func (emptyAdminConnectionManager) Connection(string) (server.ConnectionView, bool) {
	return server.ConnectionView{}, false
}
func (emptyAdminConnectionManager) DisconnectConnection(string, string) bool { return false }
func (emptyAdminConnectionManager) DisconnectCredential(uint64, string) int  { return 0 }
func (emptyAdminConnectionManager) RevalidateCredentialSource(uint64, []credential.SourceRule, string) int {
	return 0
}

type emptyAdminSessionInspector struct{}

func (emptyAdminSessionInspector) ManagementSessions(server.SessionFilter) []server.SessionView {
	return nil
}

type emptyAdminRuntimeInspector struct {
	status adminservice.ServerStatus
}

func (runtime *emptyAdminRuntimeInspector) Status() adminservice.ServerStatus { return runtime.status }
func (runtime *emptyAdminRuntimeInspector) EnableDebug() {
	runtime.status.DebugEnabled = true
}
func (runtime *emptyAdminRuntimeInspector) DisableDebug() {
	runtime.status.DebugEnabled = false
}
func (runtime *emptyAdminRuntimeInspector) RequestStop() bool { return true }
