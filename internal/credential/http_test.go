package credential

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestVerifyRequestUsesBearerAuthorization(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://relay.example", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Authorization", "Bearer hpk_cred-test_secret")
	verifier := &recordingVerifier{identity: Identity{CredentialID: "cred-test", PrincipalID: "user", MachineID: "home"}}
	identity, err := VerifyRequest(request, verifier)
	if err != nil || identity.CredentialID != "cred-test" || verifier.token != "hpk_cred-test_secret" {
		t.Fatalf("VerifyRequest() = %#v, %v, token %q", identity, err, verifier.token)
	}
}

func TestVerifyRequestRejectsMissingOrMalformedAuthorization(t *testing.T) {
	for _, authorization := range []string{"", "Basic abc", "Bearer", "Bearer a b"} {
		request, err := http.NewRequest(http.MethodGet, "https://relay.example", nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		request.Header.Set("Authorization", authorization)
		if _, err := VerifyRequest(request, &recordingVerifier{}); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("VerifyRequest(%q) error = %v, want ErrUnauthenticated", authorization, err)
		}
	}
}

type recordingVerifier struct {
	identity Identity
	token    string
	err      error
}

func (verifier *recordingVerifier) VerifyBearer(_ context.Context, token string) (Identity, error) {
	verifier.token = token
	return verifier.identity, verifier.err
}
