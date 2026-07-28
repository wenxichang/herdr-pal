package adminserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

func TestKeyHandlerIssueListShowAndPagination(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	credentialsPath := filepath.Join(t.TempDir(), "credentials.json")
	store, err := credential.LoadStore(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	connections := &trackingConnectionManager{}
	handler := newTestKeyHandler(t, store, connections, func() time.Time { return now })
	expiresAt := now.Add(24 * time.Hour).Format(time.RFC3339)
	issue := handleKeyRequest(t, handler, adminproto.MethodKeyIssue, adminproto.KeyIssueParams{
		PrincipalID: "user-a", MachineID: "home", Sources: []string{"192.168.1.10", "10.0.0.0/24"}, ExpiresAt: &expiresAt,
	})
	var issued adminproto.KeyIssueResult
	decodeKeyResult(t, issue, &issued)
	if !strings.HasPrefix(issued.Token, "hpk_1_") || issued.Credential.CredentialID != 1 || issued.Credential.PrincipalID != "user-a" || issued.Credential.MachineID != "home" {
		t.Fatalf("issue result = %#v", issued)
	}
	encodedIssue, err := adminproto.EncodeResponse(issue)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContainAny(encodedIssue, []string{"secret_sha256"}) {
		t.Fatalf("issue response leaked credential digest: %s", encodedIssue)
	}
	persisted, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), issued.Token) || !strings.Contains(string(persisted), "secret_sha256") {
		t.Fatalf("credential persistence did not keep only digest: %s", persisted)
	}

	second := handleKeyRequest(t, handler, adminproto.MethodKeyIssue, adminproto.KeyIssueParams{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"192.168.1.20"},
	})
	var secondIssued adminproto.KeyIssueResult
	decodeKeyResult(t, second, &secondIssued)

	firstPage := handleKeyRequest(t, handler, adminproto.MethodKeyList, adminproto.KeyListParams{Limit: 1})
	var firstList adminproto.KeyListResult
	decodeKeyResult(t, firstPage, &firstList)
	if firstList.ObservedAt != now || len(firstList.Items) != 1 || firstList.Items[0].CredentialID != 1 || firstList.NextPageToken == "" {
		t.Fatalf("first key page = %#v", firstList)
	}
	secondPage := handleKeyRequest(t, handler, adminproto.MethodKeyList, adminproto.KeyListParams{Limit: 1, PageToken: firstList.NextPageToken})
	var secondList adminproto.KeyListResult
	decodeKeyResult(t, secondPage, &secondList)
	if len(secondList.Items) != 1 || secondList.Items[0].CredentialID != 2 || secondList.NextPageToken != "" {
		t.Fatalf("second key page = %#v", secondList)
	}

	show := handleKeyRequest(t, handler, adminproto.MethodKeyShow, adminproto.CredentialIDParams{CredentialID: 1})
	var shown adminproto.CredentialResult
	decodeKeyResult(t, show, &shown)
	if !reflect.DeepEqual(shown.Credential, issued.Credential) {
		t.Fatalf("show result = %#v, issued = %#v", shown, issued)
	}
	if strings.Contains(string(show.Result), issued.Token) {
		t.Fatalf("show returned plaintext token: %s", show.Result)
	}
}

func TestKeyHandlerDisableEnableDeleteAndPersistenceFailureOrdering(t *testing.T) {
	store, record := seededCredentialStore(t, "192.168.1.10")
	connections := &trackingConnectionManager{}
	handler := newTestKeyHandler(t, store, connections, time.Now)

	disabledResponse := handleKeyRequest(t, handler, adminproto.MethodKeyDisable, adminproto.CredentialIDParams{CredentialID: record.CredentialID})
	var disabled adminproto.CredentialMutationResult
	decodeKeyResult(t, disabledResponse, &disabled)
	if disabled.Credential.Status != string(credential.StatusDisabled) || disabled.DisconnectedConnections != 1 || len(connections.disconnected) != 1 {
		t.Fatalf("disable result=%#v disconnects=%v", disabled, connections.disconnected)
	}

	enabledResponse := handleKeyRequest(t, handler, adminproto.MethodKeyEnable, adminproto.CredentialIDParams{CredentialID: record.CredentialID})
	var enabled adminproto.CredentialMutationResult
	decodeKeyResult(t, enabledResponse, &enabled)
	if enabled.Credential.Status != string(credential.StatusEnabled) || enabled.DisconnectedConnections != 0 || len(connections.disconnected) != 1 {
		t.Fatalf("enable result=%#v disconnects=%v", enabled, connections.disconnected)
	}

	rejectedDelete := handleKeyRequest(t, handler, adminproto.MethodKeyDelete, adminproto.KeyDeleteParams{CredentialID: record.CredentialID})
	assertKeyError(t, rejectedDelete, adminproto.CodeArgumentInvalid)
	if _, err := store.Show(record.CredentialID); err != nil {
		t.Fatalf("unconfirmed delete changed store: %v", err)
	}
	deletedResponse := handleKeyRequest(t, handler, adminproto.MethodKeyDelete, adminproto.KeyDeleteParams{CredentialID: record.CredentialID, Confirm: true})
	var deleted adminproto.KeyDeleteResult
	decodeKeyResult(t, deletedResponse, &deleted)
	if !deleted.Deleted || deleted.CredentialID != record.CredentialID || deleted.DisconnectedConnections != 1 || len(connections.disconnected) != 2 {
		t.Fatalf("delete result=%#v disconnects=%v", deleted, connections.disconnected)
	}

	failing := &failingDisableCredentialManager{CredentialManager: store, err: errors.New("disk unavailable")}
	failureConnections := &trackingConnectionManager{}
	failureHandler := newTestKeyHandler(t, failing, failureConnections, time.Now)
	failed := handleKeyRequest(t, failureHandler, adminproto.MethodKeyDisable, adminproto.CredentialIDParams{CredentialID: record.CredentialID})
	assertKeyError(t, failed, adminproto.CodeServerInternal)
	if len(failureConnections.disconnected) != 0 {
		t.Fatalf("persistence failure disconnected credentials: %v", failureConnections.disconnected)
	}
	failingDelete := &failingDeleteCredentialManager{CredentialManager: store, err: errors.New("disk unavailable")}
	deleteConnections := &trackingConnectionManager{}
	deleteHandler := newTestKeyHandler(t, failingDelete, deleteConnections, time.Now)
	failedDelete := handleKeyRequest(t, deleteHandler, adminproto.MethodKeyDelete, adminproto.KeyDeleteParams{CredentialID: record.CredentialID, Confirm: true})
	assertKeyError(t, failedDelete, adminproto.CodeServerInternal)
	if len(deleteConnections.disconnected) != 0 {
		t.Fatalf("delete persistence failure disconnected credentials: %v", deleteConnections.disconnected)
	}
}

func TestKeySourceHandlerRevalidatesOnlyAfterRestrictiveSuccess(t *testing.T) {
	store, record := seededCredentialStore(t, "192.168.1.10", "192.168.1.11")
	connections := &trackingConnectionManager{}
	handler := newTestKeyHandler(t, store, connections, time.Now)

	addedResponse := handleKeyRequest(t, handler, adminproto.MethodKeySourceAdd, adminproto.KeySourceMutationParams{CredentialID: record.CredentialID, Sources: []string{"10.0.0.0/24"}})
	var added adminproto.CredentialMutationResult
	decodeKeyResult(t, addedResponse, &added)
	if len(added.Credential.AllowedSources) != 3 || len(connections.revalidated) != 0 {
		t.Fatalf("source add result=%#v revalidated=%v", added, connections.revalidated)
	}

	removedResponse := handleKeyRequest(t, handler, adminproto.MethodKeySourceRemove, adminproto.KeySourceMutationParams{CredentialID: record.CredentialID, Sources: []string{"192.168.1.11"}})
	var removed adminproto.CredentialMutationResult
	decodeKeyResult(t, removedResponse, &removed)
	if len(connections.revalidated) != 1 || removed.DisconnectedConnections != 1 {
		t.Fatalf("source remove result=%#v revalidated=%v", removed, connections.revalidated)
	}

	setResponse := handleKeyRequest(t, handler, adminproto.MethodKeySourceSet, adminproto.KeySourceMutationParams{CredentialID: record.CredentialID, Sources: []string{"172.16.0.1-172.16.0.5"}})
	var set adminproto.CredentialMutationResult
	decodeKeyResult(t, setResponse, &set)
	if len(connections.revalidated) != 2 || set.Credential.AllowedSources[0] != "172.16.0.1-172.16.0.5" {
		t.Fatalf("source set result=%#v revalidated=%v", set, connections.revalidated)
	}

	listResponse := handleKeyRequest(t, handler, adminproto.MethodKeySourceList, adminproto.CredentialIDParams{CredentialID: record.CredentialID})
	var listed adminproto.KeySourceListResult
	decodeKeyResult(t, listResponse, &listed)
	if listed.CredentialID != record.CredentialID || len(listed.Sources) != 1 || listed.Sources[0] != "172.16.0.1-172.16.0.5" {
		t.Fatalf("source list = %#v", listed)
	}

	rejected := handleKeyRequest(t, handler, adminproto.MethodKeySourceSet, adminproto.KeySourceMutationParams{CredentialID: record.CredentialID})
	assertKeyError(t, rejected, adminproto.CodeCredentialSourceRequired)
	if len(connections.revalidated) != 2 {
		t.Fatalf("failed source update revalidated: %v", connections.revalidated)
	}
	failingSet := &failingSetSourcesCredentialManager{CredentialManager: store, err: errors.New("disk unavailable")}
	failureConnections := &trackingConnectionManager{}
	failureHandler := newTestKeyHandler(t, failingSet, failureConnections, time.Now)
	failedSet := handleKeyRequest(t, failureHandler, adminproto.MethodKeySourceSet, adminproto.KeySourceMutationParams{CredentialID: record.CredentialID, Sources: []string{"10.10.0.0/16"}})
	assertKeyError(t, failedSet, adminproto.CodeServerInternal)
	if len(failureConnections.revalidated) != 0 {
		t.Fatalf("source persistence failure revalidated: %v", failureConnections.revalidated)
	}
}

func TestKeyHandlerMapsInvalidArgumentsNotFoundAndPageToken(t *testing.T) {
	store, _ := seededCredentialStore(t, "192.168.1.10")
	handler := newTestKeyHandler(t, store, &trackingConnectionManager{}, time.Now)
	assertKeyError(t, handleRawKeyRequest(t, handler, adminproto.MethodKeyShow, []byte(`{"credential_id":"bad"}`)), adminproto.CodeArgumentInvalid)
	assertKeyError(t, handleKeyRequest(t, handler, adminproto.MethodKeyShow, adminproto.CredentialIDParams{CredentialID: 999}), adminproto.CodeCredentialNotFound)
	assertKeyError(t, handleKeyRequest(t, handler, adminproto.MethodKeyList, adminproto.KeyListParams{Limit: 501}), adminproto.CodeArgumentInvalid)
	assertKeyError(t, handleKeyRequest(t, handler, adminproto.MethodKeyList, adminproto.KeyListParams{PageToken: "not-a-token"}), adminproto.CodeArgumentInvalid)
	assertKeyError(t, handleKeyRequest(t, handler, adminproto.MethodKeyIssue, adminproto.KeyIssueParams{PrincipalID: "user", MachineID: "machine"}), adminproto.CodeCredentialSourceRequired)
}

func seededCredentialStore(t *testing.T, sources ...string) (*credential.Store, credential.Record) {
	t.Helper()
	store, err := credential.LoadStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, record, err := store.Issue("user-a", "home", sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, record
}

func newTestKeyHandler(t *testing.T, credentials CredentialManager, connections ConnectionManager, now func() time.Time) *KeyHandler {
	t.Helper()
	handler, err := NewKeyHandler(credentials, connections, slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func handleKeyRequest(t *testing.T, handler *KeyHandler, method adminproto.Method, params any) adminproto.Response {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return handleRawKeyRequest(t, handler, method, encoded)
}

func handleRawKeyRequest(t *testing.T, handler *KeyHandler, method adminproto.Method, params []byte) adminproto.Response {
	t.Helper()
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: method, Params: params})
	if err != nil {
		t.Fatalf("Handle(%s) error = %v", method, err)
	}
	return result.Response
}

func decodeKeyResult(t *testing.T, response adminproto.Response, destination any) {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("response error = %#v", response.Error)
	}
	if err := json.Unmarshal(response.Result, destination); err != nil {
		t.Fatal(err)
	}
}

func assertKeyError(t *testing.T, response adminproto.Response, code adminproto.ErrorCode) {
	t.Helper()
	if response.Error == nil || response.Error.Code != code {
		t.Fatalf("response = %#v, want error %q", response, code)
	}
}

func bytesContainAny(value []byte, fragments []string) bool {
	for _, fragment := range fragments {
		if strings.Contains(string(value), fragment) {
			return true
		}
	}
	return false
}

type trackingConnectionManager struct {
	ConnectionManager
	disconnected []uint64
	revalidated  []uint64
}

func (manager *trackingConnectionManager) DisconnectCredential(credentialID uint64, _ string) int {
	manager.disconnected = append(manager.disconnected, credentialID)
	return 1
}

func (manager *trackingConnectionManager) RevalidateCredentialSource(credentialID uint64, _ []credential.SourceRule, _ string) int {
	manager.revalidated = append(manager.revalidated, credentialID)
	return 1
}

type failingDisableCredentialManager struct {
	CredentialManager
	err error
}

func (manager *failingDisableCredentialManager) Disable(uint64) (credential.Record, error) {
	return credential.Record{}, manager.err
}

type failingDeleteCredentialManager struct {
	CredentialManager
	err error
}

func (manager *failingDeleteCredentialManager) Delete(uint64) (credential.Record, error) {
	return credential.Record{}, manager.err
}

type failingSetSourcesCredentialManager struct {
	CredentialManager
	err error
}

func (manager *failingSetSourcesCredentialManager) SetSources(uint64, []string) (credential.Record, error) {
	return credential.Record{}, manager.err
}
