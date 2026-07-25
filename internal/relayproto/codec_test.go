package relayproto

import (
	"bytes"
	"errors"
	"testing"
)

func TestProtocolVersionIncludesStableExecutePushTarget(t *testing.T) {
	if ProtocolVersion != 2 {
		t.Fatalf("ProtocolVersion = %d, want 2", ProtocolVersion)
	}
}

func TestFrameRoundTripUsesStrictVersionedEnvelope(t *testing.T) {
	frame, err := NewFrame(TypeClientHello, "request-1", ClientHello{
		UserID: "zhangsan", MachineID: "home-mac", ClientVersion: "v0.1.0",
	})
	if err != nil {
		t.Fatalf("NewFrame() error = %v", err)
	}
	encoded, err := Encode(frame)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.Protocol != ProtocolVersion || decoded.Type != TypeClientHello || decoded.RequestID != "request-1" {
		t.Fatalf("Decode() = %#v", decoded)
	}
	payload, err := DecodePayload[ClientHello](decoded)
	if err != nil {
		t.Fatalf("DecodePayload() error = %v", err)
	}
	if payload.UserID != "zhangsan" || payload.MachineID != "home-mac" || payload.ClientVersion != "v0.1.0" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestDecodeRejectsUnknownFieldTrailingJSONAndVersionMismatch(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want error
	}{
		{name: "unknown field", raw: []byte(`{"protocol":2,"type":"ping","payload":{"nonce":"n"},"extra":1}`), want: ErrInvalidFrame},
		{name: "trailing json", raw: []byte(`{"protocol":2,"type":"ping","payload":{"nonce":"n"}} {}`), want: ErrInvalidFrame},
		{name: "protocol mismatch", raw: []byte(`{"protocol":1,"type":"ping","payload":{"nonce":"n"}}`), want: ErrProtocolMismatch},
		{name: "unknown type", raw: []byte(`{"protocol":2,"type":"future","payload":{}}`), want: ErrInvalidFrame},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode(test.raw); !errors.Is(err, test.want) {
				t.Fatalf("Decode() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDecodePayloadRejectsUnknownFieldAndTrailingJSON(t *testing.T) {
	for _, test := range []struct {
		raw     []byte
		wantErr bool
	}{
		{raw: []byte(`{"protocol":2,"type":"ping","payload":{"nonce":"n","extra":1}}`), wantErr: true},
		{raw: []byte(`{"protocol":2,"type":"ping","payload":{"nonce":"n"}}`)},
	} {
		frame, err := Decode(test.raw)
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		_, err = DecodePayload[struct {
			Nonce string `json:"nonce"`
		}](frame)
		if test.wantErr && !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("DecodePayload() error = %v", err)
		}
		if !test.wantErr && err != nil {
			t.Fatalf("DecodePayload() error = %v", err)
		}
	}
}

func TestDecodeRejectsFrameLargerThanLimit(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), MaxFrameBytes+1)
	if _, err := Decode(raw); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("Decode() error = %v", err)
	}
}

func TestProtocolErrorDoesNotEchoUnsafeMessage(t *testing.T) {
	err := NewProtocolError(CodeInvalidFrame, "secret prompt content", true)
	if strings := err.Error(); bytes.Contains([]byte(strings), []byte("secret prompt content")) {
		t.Fatalf("Error() leaked unsafe message: %q", strings)
	}
}
