package adminauth

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestAutomationTokenRoundTrip(t *testing.T) {
	now := time.Unix(10, 0).UTC()
	token, record, err := GenerateAutomationToken(bytes.NewReader(bytes.Repeat([]byte{2}, 40)), now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "hpa_") || len(record.TokenID) != 16 || !VerifyAutomationToken(record, token) {
		t.Fatalf("token=%q record=%#v", token, record)
	}
	if record.SecretSHA256 == "" || strings.Contains(record.SecretSHA256, token) || !record.Enabled || record.CreatedAt != now || record.UpdatedAt != now {
		t.Fatalf("record = %#v", record)
	}
	if VerifyAutomationToken(record, token+"x") {
		t.Fatal("modified token verified")
	}
}

func TestAutomationTokenRejectsMalformedValues(t *testing.T) {
	_, record, err := GenerateAutomationToken(bytes.NewReader(bytes.Repeat([]byte{3}, 40)), time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "hpa_bad", "hpa_ABCDEF0123456789_secret", "hpa_0000000000000000_short"} {
		if VerifyAutomationToken(record, value) {
			t.Fatalf("VerifyAutomationToken accepted %q", value)
		}
	}
}

func TestAutomationTokenSecretMayContainDelimiter(t *testing.T) {
	randomData := append(make([]byte, automationTokenIDBytes), bytes.Repeat([]byte{0xff}, automationSecretBytes)...)
	token, record, err := GenerateAutomationToken(bytes.NewReader(randomData), time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(token, "_") <= 2 {
		t.Fatalf("test token did not contain a delimiter-like Secret: %q", token)
	}
	if !VerifyAutomationToken(record, token) {
		t.Fatal("token with underscore in Secret did not verify")
	}
}
