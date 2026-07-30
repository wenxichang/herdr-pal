package audit

import (
	"strings"
	"testing"
)

func TestRedactorRemovesConfiguredAndKnownCredentials(t *testing.T) {
	redactor := NewRedactor([]string{"bot-secret-value", "Bearer collector-token"})
	input := strings.Join([]string{
		"prompt bot-secret-value",
		"Authorization: Bearer user-access-token",
		"Cookie: session=secret-cookie",
		"Set-Cookie: refresh=secret-refresh; Secure",
		"machine hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"automation hpa_0123456789abcdef_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"collector Bearer collector-token",
	}, "\n")
	output := redactor.Redact(input)
	for _, forbidden := range []string{
		"bot-secret-value", "user-access-token", "secret-cookie", "secret-refresh",
		"hpk_12_", "collector-token",
		"hpa_0123456789abcdef_",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("Redact() leaked %q in %q", forbidden, output)
		}
	}
	if strings.Count(output, RedactedValue) < 5 {
		t.Fatalf("Redact() = %q", output)
	}
}

func TestRedactorPreservesOrdinaryText(t *testing.T) {
	input := "请分析 Authorization 设计、Cookie 规范和普通 token 字样"
	if output := NewRedactor(nil).Redact(input); output != input {
		t.Fatalf("Redact() = %q, want %q", output, input)
	}
}

func TestRedactorReplacesLongerConfiguredValuesFirst(t *testing.T) {
	redactor := NewRedactor([]string{"secret", "secret-value"})
	if output := redactor.Redact("secret-value"); output != RedactedValue {
		t.Fatalf("Redact() = %q", output)
	}
}
