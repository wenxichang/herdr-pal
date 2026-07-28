package hprp

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	envelope, err := NewEnvelope(TypeHelloClient, "hello-1", "", false, map[string]any{"name": "pal"})
	if err != nil {
		t.Fatalf("NewEnvelope() error = %v", err)
	}
	encoded, err := Encode(envelope)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.Protocol != ProtocolVersion || decoded.Type != TypeHelloClient || decoded.ID != "hello-1" {
		t.Fatalf("Decode() = %#v", decoded)
	}
	payload, err := DecodePayload[struct {
		Name string `json:"name"`
	}](decoded)
	if err != nil || payload.Name != "pal" {
		t.Fatalf("DecodePayload() = %#v, %v", payload, err)
	}
}

func TestDecodeIgnoresUnknownOptionalFields(t *testing.T) {
	decoded, err := Decode([]byte(`{
		"protocol":"HPRP/1",
		"type":"hello.client",
		"id":"hello-1",
		"future_flag":true,
		"payload":{"name":"pal","future_value":1}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	payload, err := DecodePayload[struct {
		Name string `json:"name"`
	}](decoded)
	if err != nil || payload.Name != "pal" {
		t.Fatalf("DecodePayload() = %#v, %v", payload, err)
	}
}

func TestDecodeRejectsDuplicateObjectFields(t *testing.T) {
	tests := []string{
		`{"protocol":"HPRP/1","protocol":"HPRP/1","type":"hello.client","id":"hello-1","payload":{}}`,
		`{"protocol":"HPRP/1","type":"hello.client","id":"hello-1","payload":{"name":"a","name":"b"}}`,
	}
	for _, input := range tests {
		if _, err := Decode([]byte(input)); !errors.Is(err, ErrDuplicateField) {
			t.Fatalf("Decode(%s) error = %v, want ErrDuplicateField", input, err)
		}
	}
}

func TestDecodeRejectsInvalidEnvelope(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "protocol", input: `{"protocol":"HPRP/2","type":"hello.client","id":"hello-1","payload":{}}`, want: ErrProtocolMismatch},
		{name: "type", input: `{"protocol":"HPRP/1","type":"HelloClient","id":"hello-1","payload":{}}`, want: ErrInvalidMessage},
		{name: "id", input: `{"protocol":"HPRP/1","type":"hello.client","id":"","payload":{}}`, want: ErrInvalidMessage},
		{name: "payload null", input: `{"protocol":"HPRP/1","type":"hello.client","id":"hello-1","payload":null}`, want: ErrInvalidMessage},
		{name: "payload array", input: `{"protocol":"HPRP/1","type":"hello.client","id":"hello-1","payload":[]}`, want: ErrInvalidMessage},
		{name: "trailing", input: `{"protocol":"HPRP/1","type":"hello.client","id":"hello-1","payload":{}} {}`, want: ErrInvalidMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Decode([]byte(test.input)); !errors.Is(err, test.want) {
				t.Fatalf("Decode() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDecodeRejectsOversizedMessage(t *testing.T) {
	input := bytes.Repeat([]byte{'x'}, MaxMessageBytes+1)
	if _, err := Decode(input); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Decode() error = %v, want ErrMessageTooLarge", err)
	}
}

func TestNewEnvelopeRejectsNonObjectPayload(t *testing.T) {
	if _, err := NewEnvelope(TypeHelloClient, "hello-1", "", false, "not-object"); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("NewEnvelope() error = %v, want ErrInvalidMessage", err)
	}
}

func TestEncodeRejectsOversizedMessage(t *testing.T) {
	envelope, err := NewEnvelope(TypeHelloClient, "hello-1", "", false, map[string]any{
		"value": strings.Repeat("x", MaxMessageBytes),
	})
	if err != nil {
		t.Fatalf("NewEnvelope() error = %v", err)
	}
	if _, err := Encode(envelope); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Encode() error = %v, want ErrMessageTooLarge", err)
	}
}
