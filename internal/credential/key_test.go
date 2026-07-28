package credential

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueCreatesMachineKeyWithoutPersistingSecret(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	random := bytes.NewReader(bytes.Repeat([]byte{0x42}, issueRandomBytes))
	token, record, err := Issue("wecom-user", "office-pc", now, random)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if !strings.HasPrefix(token, "hpk_") || record.CredentialID == "" || record.SecretSHA256 == "" {
		t.Fatalf("Issue() = %q, %#v", token, record)
	}
	if strings.Contains(record.SecretSHA256, token) || len(record.SecretSHA256) != 64 {
		t.Fatalf("record persisted unsafe secret: %#v", record)
	}
	if record.PrincipalID != "wecom-user" || record.MachineID != "office-pc" || record.Status != StatusActive || !record.CreatedAt.Equal(now) {
		t.Fatalf("record = %#v", record)
	}
	identity, err := VerifyRecord(record, token, now)
	if err != nil {
		t.Fatalf("VerifyRecord() error = %v", err)
	}
	if identity.CredentialID != record.CredentialID || identity.PrincipalID != record.PrincipalID || identity.MachineID != record.MachineID {
		t.Fatalf("VerifyRecord() = %#v", identity)
	}
}

func TestBearerCredentialIDParsesIssuedToken(t *testing.T) {
	token, record, err := Issue("user", "home", time.Now(), bytes.NewReader(bytes.Repeat([]byte{0x24}, issueRandomBytes)))
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	credentialID, err := BearerCredentialID(token)
	if err != nil || credentialID != record.CredentialID {
		t.Fatalf("BearerCredentialID() = %q, %v", credentialID, err)
	}
}

func TestBearerCredentialIDAllowsURLSafeUnderscoreInSecret(t *testing.T) {
	randomData := append(bytes.Repeat([]byte{0x42}, 16), bytes.Repeat([]byte{0xff}, 32)...)
	token, record, err := Issue("user", "home", time.Now(), bytes.NewReader(randomData))
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	credentialID, err := BearerCredentialID(token)
	if err != nil || credentialID != record.CredentialID {
		t.Fatalf("BearerCredentialID() = %q, %v, token %q", credentialID, err, token)
	}
}

func TestVerifyRecordRejectsAllUnusableCredentialsAsUnauthenticated(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	token, record, err := Issue("user", "home", now, bytes.NewReader(bytes.Repeat([]byte{0x11}, issueRandomBytes)))
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	expired := now.Add(-time.Second)
	for _, test := range []struct {
		name   string
		record Record
		token  string
	}{
		{name: "tampered", record: record, token: token + "x"},
		{name: "revoked", record: func() Record { copy := record; copy.Status = StatusRevoked; return copy }(), token: token},
		{name: "expired", record: func() Record { copy := record; copy.ExpiresAt = &expired; return copy }(), token: token},
		{name: "wrong id", record: func() Record { copy := record; copy.CredentialID = "cred-other"; return copy }(), token: token},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := VerifyRecord(test.record, test.token, now); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("VerifyRecord() error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestIssueRejectsInvalidIdentity(t *testing.T) {
	if _, _, err := Issue("", "home", time.Now(), bytes.NewReader(bytes.Repeat([]byte{1}, issueRandomBytes))); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Issue() error = %v, want ErrInvalidRecord", err)
	}
	if _, _, err := Issue("user", "bad machine", time.Now(), bytes.NewReader(bytes.Repeat([]byte{1}, issueRandomBytes))); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Issue() error = %v, want ErrInvalidRecord", err)
	}
}
