package credential

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"testing"
)

func TestVerifyRequestUsesBearerAndRealTCPPeer(t *testing.T) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://relay.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.RemoteAddr = "192.168.1.20:4321"
	request.Header.Set("Authorization", "Bearer hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	request.Header.Set("X-Forwarded-For", "10.0.0.1")
	request.Header.Set("Forwarded", "for=10.0.0.2")
	verifier := &recordingVerifier{identity: Identity{CredentialID: 12, PrincipalID: "user", MachineID: "home"}}
	identity, err := VerifyRequest(request, verifier)
	if err != nil || identity.CredentialID != 12 {
		t.Fatalf("VerifyRequest() = %#v, %v", identity, err)
	}
	if verifier.token != request.Header.Get("Authorization")[len("Bearer "):] || verifier.source != netip.MustParseAddr("192.168.1.20") {
		t.Fatalf("verifier received token %q source %s", verifier.token, verifier.source)
	}
}

func TestVerifyRequestNormalizesIPv6ZoneAndMappedIPv4(t *testing.T) {
	for _, test := range []struct {
		remote string
		want   netip.Addr
	}{
		{remote: "[fe80::1%en0]:1234", want: netip.MustParseAddr("fe80::1")},
		{remote: "[::ffff:192.168.1.20]:1234", want: netip.MustParseAddr("192.168.1.20")},
	} {
		request, err := http.NewRequest(http.MethodGet, "https://relay.example", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.RemoteAddr = test.remote
		request.Header.Set("Authorization", "Bearer hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		verifier := &recordingVerifier{identity: Identity{CredentialID: 1}}
		if _, err := VerifyRequest(request, verifier); err != nil {
			t.Fatalf("VerifyRequest(%q) error = %v", test.remote, err)
		}
		if verifier.source != test.want {
			t.Fatalf("source = %s, want %s", verifier.source, test.want)
		}
	}
}

func TestVerifyRequestRejectsMalformedAuthorizationAndRemoteAddress(t *testing.T) {
	tests := []struct {
		authorization string
		remote        string
	}{
		{authorization: "", remote: "127.0.0.1:1"},
		{authorization: "Basic abc", remote: "127.0.0.1:1"},
		{authorization: "Bearer", remote: "127.0.0.1:1"},
		{authorization: "Bearer a b", remote: "127.0.0.1:1"},
		{authorization: "Bearer hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", remote: "invalid"},
		{authorization: "Bearer hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", remote: "hostname:123"},
	}
	for _, test := range tests {
		request, err := http.NewRequest(http.MethodGet, "https://relay.example", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.RemoteAddr = test.remote
		request.Header.Set("Authorization", test.authorization)
		if _, err := VerifyRequest(request, &recordingVerifier{}); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("VerifyRequest(%q, %q) error = %v", test.authorization, test.remote, err)
		}
	}
}

type recordingVerifier struct {
	identity Identity
	token    string
	source   netip.Addr
	err      error
}

func (verifier *recordingVerifier) VerifyBearer(_ context.Context, token string, source netip.Addr) (Identity, error) {
	verifier.token = token
	verifier.source = source
	return verifier.identity, verifier.err
}
