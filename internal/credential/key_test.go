package credential

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestValidatePrincipalID(t *testing.T) {
	for _, value := range []string{"user-a", "企业微信用户"} {
		if err := ValidatePrincipalID(value); err != nil {
			t.Fatalf("ValidatePrincipalID(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"", " user-a", "user\n"} {
		if err := ValidatePrincipalID(value); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("ValidatePrincipalID(%q) = %v", value, err)
		}
	}
}

func TestIssueCreatesEnabledMachineKeyWithNumericIDAndSources(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(24 * time.Hour)
	rules := []SourceRule{"192.168.1.0/24", "2001:db8::1"}
	token, record, err := Issue(12, "wecom-user", "office-pc", rules, &expiresAt, now, bytes.NewReader(bytes.Repeat([]byte{0x42}, issueRandomBytes)))
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if !strings.HasPrefix(token, "hpk_12_") || record.CredentialID != 12 || record.SecretSHA256 == "" {
		t.Fatalf("Issue() = %q, %#v", token, record)
	}
	if strings.Contains(record.SecretSHA256, token) || len(record.SecretSHA256) != 64 {
		t.Fatalf("record persisted unsafe secret: %#v", record)
	}
	if record.PrincipalID != "wecom-user" || record.MachineID != "office-pc" || record.Status != StatusEnabled {
		t.Fatalf("record identity = %#v", record)
	}
	if !record.CreatedAt.Equal(now) || !record.UpdatedAt.Equal(now) || record.ExpiresAt == nil || !record.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("record timestamps = %#v", record)
	}
	if len(record.AllowedSources) != 2 || record.AllowedSources[0] != rules[0] || record.AllowedSources[1] != rules[1] {
		t.Fatalf("record sources = %#v", record.AllowedSources)
	}
	identity, err := VerifyRecord(record, token, now, netip.MustParseAddr("192.168.1.20"))
	if err != nil {
		t.Fatalf("VerifyRecord() error = %v", err)
	}
	if identity.CredentialID != 12 || identity.PrincipalID != record.PrincipalID || identity.MachineID != record.MachineID {
		t.Fatalf("VerifyRecord() = %#v", identity)
	}
}

func TestBearerCredentialIDParsesDecimalIDAndURLSafeSecret(t *testing.T) {
	randomData := bytes.Repeat([]byte{0xff}, issueRandomBytes)
	token, _, err := Issue(42, "user", "home", []SourceRule{"0.0.0.0/0"}, nil, time.Now(), bytes.NewReader(randomData))
	if err != nil {
		t.Fatal(err)
	}
	credentialID, err := BearerCredentialID(token)
	if err != nil || credentialID != 42 || !strings.Contains(token, "_") {
		t.Fatalf("BearerCredentialID() = %d, %v, token %q", credentialID, err, token)
	}
	for _, invalid := range []string{
		"plain-secret",
		"hpk_0_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"hpk_01_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"hpk_-1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"hpk_1_short",
		"hpk_1_!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!",
	} {
		if _, err := BearerCredentialID(invalid); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("BearerCredentialID(%q) error = %v", invalid, err)
		}
	}
}

func TestVerifyRecordRejectsAllUnusableCredentialsAsUnauthenticated(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	token, record, err := Issue(7, "user", "home", []SourceRule{"192.168.1.0/24"}, nil, now, bytes.NewReader(bytes.Repeat([]byte{0x11}, issueRandomBytes)))
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(time.Second)
	expiringToken, expiringRecord, err := Issue(9, "user", "home", []SourceRule{"192.168.1.0/24"}, &expiresAt, now, bytes.NewReader(bytes.Repeat([]byte{0x22}, issueRandomBytes)))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		record Record
		token  string
		source netip.Addr
		at     time.Time
	}{
		{name: "tampered", record: record, token: token + "x", source: netip.MustParseAddr("192.168.1.2"), at: now},
		{name: "disabled", record: func() Record { copy := record; copy.Status = StatusDisabled; return copy }(), token: token, source: netip.MustParseAddr("192.168.1.2"), at: now},
		{name: "expired", record: expiringRecord, token: expiringToken, source: netip.MustParseAddr("192.168.1.2"), at: now.Add(2 * time.Second)},
		{name: "wrong id", record: func() Record { copy := record; copy.CredentialID = 8; return copy }(), token: token, source: netip.MustParseAddr("192.168.1.2"), at: now},
		{name: "source mismatch", record: record, token: token, source: netip.MustParseAddr("10.0.0.1"), at: now},
		{name: "invalid source", record: record, token: token, source: netip.Addr{}, at: now},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := VerifyRecord(test.record, test.token, test.at, test.source); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("VerifyRecord() error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestIssueRejectsInvalidRecordInputs(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	random := func() *bytes.Reader { return bytes.NewReader(bytes.Repeat([]byte{1}, issueRandomBytes)) }
	tests := []struct {
		name      string
		id        uint64
		principal string
		machine   string
		sources   []SourceRule
		expiresAt *time.Time
	}{
		{name: "zero id", id: 0, principal: "user", machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "empty principal", id: 1, machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "principal with leading space", id: 1, principal: " user", machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "principal with trailing space", id: 1, principal: "user ", machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "principal with control", id: 1, principal: "user\nother", machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "bad machine", id: 1, principal: "user", machine: "bad machine", sources: []SourceRule{"127.0.0.1"}, expiresAt: &future},
		{name: "empty sources", id: 1, principal: "user", machine: "home", expiresAt: &future},
		{name: "noncanonical source", id: 1, principal: "user", machine: "home", sources: []SourceRule{"192.168.1.42/24"}, expiresAt: &future},
		{name: "past expiration", id: 1, principal: "user", machine: "home", sources: []SourceRule{"127.0.0.1"}, expiresAt: &past},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := Issue(test.id, test.principal, test.machine, test.sources, test.expiresAt, now, random()); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("Issue() error = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestIssueAcceptsNoExpirationAndDoesNotAliasSources(t *testing.T) {
	rules := []SourceRule{"127.0.0.1"}
	_, record, err := Issue(1, "user", "home", rules, nil, time.Now(), bytes.NewReader(bytes.Repeat([]byte{1}, issueRandomBytes)))
	if err != nil {
		t.Fatal(err)
	}
	rules[0] = "10.0.0.1"
	if record.ExpiresAt != nil || record.AllowedSources[0] != "127.0.0.1" {
		t.Fatalf("record = %#v", record)
	}
}
