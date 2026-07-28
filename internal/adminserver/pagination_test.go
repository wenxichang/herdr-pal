package adminserver

import (
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestKeyPageTokenRejectsCrossMethodAndTampering(t *testing.T) {
	token, err := encodeCredentialPageToken(adminproto.MethodConnectionList, 42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredentialPageToken(token, adminproto.MethodKeyList); err == nil {
		t.Fatal("page token was accepted by a different method")
	}
	if _, err := decodeCredentialPageToken(token+"x", adminproto.MethodConnectionList); err == nil {
		t.Fatal("tampered page token was accepted")
	}
}
