package adminproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMethodsContainsHPAPVersionOneSurface(t *testing.T) {
	want := []Method{
		MethodServerStatus,
		MethodServerStop,
		MethodServerDebugEnable,
		MethodServerDebugDisable,
		MethodKeyIssue,
		MethodKeyList,
		MethodKeyShow,
		MethodKeyEnable,
		MethodKeyDisable,
		MethodKeyDelete,
		MethodKeySourceList,
		MethodKeySourceAdd,
		MethodKeySourceRemove,
		MethodKeySourceSet,
		MethodConnectionList,
		MethodConnectionShow,
		MethodConnectionDisconnect,
		MethodSessionList,
	}
	if got := Methods(); len(got) != len(want) {
		t.Fatalf("Methods() length = %d, want %d", len(got), len(want))
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("Methods()[%d] = %q, want %q", index, got[index], want[index])
			}
			if !IsKnownMethod(got[index]) {
				t.Fatalf("IsKnownMethod(%q) = false", got[index])
			}
		}
	}
	if IsKnownMethod("key.rotate") {
		t.Fatal("IsKnownMethod() accepted unsupported method")
	}
}

func TestKeyCredentialJSONNeverContainsStoredSecretDigest(t *testing.T) {
	expiresAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	credential := Credential{
		CredentialID: 1, PrincipalID: "user-a", MachineID: "home", Status: "enabled",
		AllowedSources: []string{"192.168.1.10"}, ExpiresAt: &expiresAt,
	}
	encoded, err := json.Marshal(CredentialResult{Credential: credential})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || !strings.Contains(string(encoded), `"credential_id":1`) {
		t.Fatalf("credential JSON = %s", encoded)
	}
}

func TestServerAndSessionManagementJSONContainsOnlyDeclaredSafeFields(t *testing.T) {
	status := ServerStatusResult{
		Version: "v1", WeCom: WeComStatus{State: "reconnecting", LastErrorType: "dns"},
		TLS: TLSStatus{Mode: "automatic", SHA256Fingerprint: strings.Repeat("a", 64)},
	}
	encodedStatus, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "token", "response_body", "terminal"} {
		if strings.Contains(strings.ToLower(string(encodedStatus)), forbidden) {
			t.Fatalf("server status JSON leaked %q: %s", forbidden, encodedStatus)
		}
	}
	session := Session{
		PrincipalID: "user", Number: 1,
		Target:    SessionTarget{MachineID: "home", SlotID: "w1:p1", SessionID: "session-1"},
		Workspace: "workspace", Tab: "main", Agent: "codex", DisplayAgent: "Codex", Pane: "w1:p1", Title: "title", Status: "done", StatusLabel: "done ✅",
	}
	encodedSession, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"machine_id":"home"`, `"slot_id":"w1:p1"`, `"session_id":"session-1"`, `"pane":"w1:p1"`, `"status_label":"done ✅"`} {
		if !strings.Contains(string(encodedSession), want) {
			t.Fatalf("session JSON = %s, want %s", encodedSession, want)
		}
	}
}

func TestErrorCodesContainsHPAPVersionOneSurface(t *testing.T) {
	want := []ErrorCode{
		CodeProtocolInvalidRequest,
		CodeProtocolUnsupportedVersion,
		CodeProtocolUnsupportedMethod,
		CodeProtocolLimitExceeded,
		CodeArgumentInvalid,
		CodeCredentialNotFound,
		CodeCredentialConflict,
		CodeCredentialSourceRequired,
		CodeCredentialSourceInvalid,
		CodeConnectionNotFound,
		CodeServerBusy,
		CodeServerInternal,
	}
	if got := ErrorCodes(); len(got) != len(want) {
		t.Fatalf("ErrorCodes() length = %d, want %d", len(got), len(want))
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("ErrorCodes()[%d] = %q, want %q", index, got[index], want[index])
			}
		}
	}
}
