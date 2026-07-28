package adminclient

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestFormatJSONWritesOneDocument(t *testing.T) {
	var output bytes.Buffer
	value := adminproto.KeyListResult{Items: []adminproto.Credential{{CredentialID: 1, PrincipalID: "user", MachineID: "home"}}}
	if err := FormatJSON(&output, value); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var decoded adminproto.KeyListResult
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("JSON output contains more than one document: %q", output.String())
	}
}

func TestFormatHumanTablesAndEmptyMessagesAreStable(t *testing.T) {
	tests := []struct {
		method adminproto.Method
		value  any
		want   []string
	}{
		{method: adminproto.MethodKeyList, value: adminproto.KeyListResult{}, want: []string{"没有 Key。"}},
		{method: adminproto.MethodConnectionList, value: adminproto.ConnectionListResult{}, want: []string{"没有活动连接。"}},
		{method: adminproto.MethodSessionList, value: adminproto.SessionListResult{}, want: []string{"没有在线 Agent 会话。"}},
		{method: adminproto.MethodKeyList, value: adminproto.KeyListResult{Items: []adminproto.Credential{{CredentialID: 1, PrincipalID: "user", MachineID: "home", Status: "enabled", AllowedSources: []string{"192.168.1.10"}}}}, want: []string{"ID", "PRINCIPAL", "MACHINE", "STATUS", "SOURCES", "1", "user", "home"}},
		{method: adminproto.MethodConnectionList, value: adminproto.ConnectionListResult{Items: []adminproto.Connection{{ConnectionID: "c-1", PrincipalID: "user", MachineID: "home", SourceIP: "10.0.0.1", Ready: true}}}, want: []string{"CONNECTION", "PRINCIPAL", "MACHINE", "SOURCE", "READY", "c-1"}},
		{method: adminproto.MethodSessionList, value: adminproto.SessionListResult{Items: []adminproto.Session{{PrincipalID: "user", Number: 1, Target: adminproto.SessionTarget{MachineID: "home"}, WorkspaceLabel: "workspace/main", DisplayAgent: "Codex", Pane: "w1:p1", StatusLabel: "future 🧪"}}}, want: []string{"PRINCIPAL", "#", "MACHINE", "WORKSPACE", "AGENT", "PANE", "STATUS", "future 🧪"}},
	}
	for _, test := range tests {
		var output bytes.Buffer
		if err := FormatHuman(&output, test.method, test.value); err != nil {
			t.Fatalf("FormatHuman(%s) error = %v", test.method, err)
		}
		for _, want := range test.want {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("FormatHuman(%s) = %q, want %q", test.method, output.String(), want)
			}
		}
	}
}

func TestFormatHumanPrintsTokenOnlyForIssueResult(t *testing.T) {
	credential := adminproto.Credential{CredentialID: 1, PrincipalID: "user", MachineID: "home", Status: "enabled", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	var issueOutput bytes.Buffer
	if err := FormatHuman(&issueOutput, adminproto.MethodKeyIssue, adminproto.KeyIssueResult{Token: "hpk_1_secret-once", Credential: credential}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issueOutput.String(), "hpk_1_secret-once") {
		t.Fatalf("issue output = %q", issueOutput.String())
	}
	var showOutput bytes.Buffer
	if err := FormatHuman(&showOutput, adminproto.MethodKeyShow, adminproto.CredentialResult{Credential: credential}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(showOutput.String(), "hpk_") {
		t.Fatalf("show output leaked token: %q", showOutput.String())
	}
}
