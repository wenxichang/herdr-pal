package server

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeServerErrorReasonRedactsCredentials(t *testing.T) {
	reason := safeServerErrorReason(errors.New(
		"machine hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA " +
			"automation hpa_0123456789abcdef_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
	))
	if strings.Contains(reason, "hpk_") || strings.Contains(reason, "hpa_") {
		t.Fatalf("safeServerErrorReason() leaked credential: %q", reason)
	}
	if strings.Count(reason, "[REDACTED]") != 2 {
		t.Fatalf("safeServerErrorReason() = %q, want two redactions", reason)
	}
}
