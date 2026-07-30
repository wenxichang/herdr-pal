package adminauth

import (
	"bytes"
	"strings"
	"testing"
)

func TestArgon2idCodecHashesAndVerifiesPassword(t *testing.T) {
	codec := NewArgon2idCodec()
	hash, err := codec.Hash("correct horse battery", bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash = %q", hash)
	}
	if ok, err := codec.Verify(hash, "correct horse battery"); err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	if ok, err := codec.Verify(hash, "incorrect horse battery"); err != nil || ok {
		t.Fatalf("Verify(wrong) = %v, %v", ok, err)
	}
}

func TestArgon2idCodecRejectsInvalidPasswordAndUnsafeEncoding(t *testing.T) {
	codec := NewArgon2idCodec()
	for _, password := range []string{"short", strings.Repeat("x", 257)} {
		if _, err := codec.Hash(password, bytes.NewReader(bytes.Repeat([]byte{1}, 16))); err == nil {
			t.Fatalf("Hash accepted password length %d", len(password))
		}
	}
	unsafe := "$argon2id$v=19$m=1048576,t=3,p=2$AQEBAQEBAQEBAQEBAQEBAQ$AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"
	if _, err := codec.Verify(unsafe, "correct horse battery"); err == nil {
		t.Fatal("Verify accepted unsafe Argon2 parameters")
	}
}
