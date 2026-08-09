package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPrepareEventAssignsStableIdentityAndHashes(t *testing.T) {
	input := Event{
		EventName:   EventNameUserInput,
		Timestamp:   time.Unix(10, 0),
		PrincipalID: "zhangsan-token-like-id",
		MessageID:   "message-secret-id",
		RequestID:   "request-secret-id",
		SessionID:   "session-secret-id",
		Body:        "hello",
	}
	event, err := PrepareEvent(input, time.Unix(11, 0), bytes.NewReader(bytes.Repeat([]byte{0xab}, 16)))
	if err != nil {
		t.Fatalf("PrepareEvent() error = %v", err)
	}
	if event.SchemaVersion != 1 || event.EventID != strings.Repeat("ab", 16) {
		t.Fatalf("identity = %#v", event)
	}
	if event.ObservedTimestamp != time.Unix(11, 0) || event.PrincipalID != input.PrincipalID || event.ContentBytes != 5 {
		t.Fatalf("event = %#v", event)
	}
	if event.MessageIDHash == "" || event.RequestIDHash == "" || event.SessionIDHash == "" {
		t.Fatalf("hashes = %#v", event)
	}
	if event.MessageID != "" || event.RequestID != "" || event.SessionID != "" {
		t.Fatalf("raw identifiers leaked = %#v", event)
	}
}

func TestPrepareEventPreservesExistingEventIDAcrossOutputs(t *testing.T) {
	input := Event{EventID: strings.Repeat("cd", 16), EventName: EventNameTerminalOutput, Body: "terminal"}
	event, err := PrepareEvent(input, time.Unix(20, 0), bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("PrepareEvent() error = %v", err)
	}
	if event.EventID != input.EventID {
		t.Fatalf("event id = %q", event.EventID)
	}
}

func TestPrepareEventAcceptsMachineRegistration(t *testing.T) {
	event, err := PrepareEvent(Event{
		EventName:   EventNameMachineRegistration,
		PrincipalID: "user-a",
		MachineID:   "office-laptop",
		Action:      "request",
		Outcome:     "pending",
		Body:        "机器注册申请 office-laptop",
	}, time.Unix(30, 0), bytes.NewReader(bytes.Repeat([]byte{0xcd}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if event.EventName != EventNameMachineRegistration || event.SchemaVersion != 1 || event.EventID == "" {
		t.Fatalf("event = %#v", event)
	}
}

func TestPrepareEventRejectsInvalidNameAndRandomSource(t *testing.T) {
	if _, err := PrepareEvent(Event{EventName: "unknown"}, time.Now(), bytes.NewReader(make([]byte, 16))); err == nil {
		t.Fatal("PrepareEvent() accepted unknown event name")
	}
	if _, err := PrepareEvent(Event{EventName: EventNameUserInput}, time.Now(), bytes.NewReader(nil)); err == nil {
		t.Fatal("PrepareEvent() accepted exhausted random source")
	}
}
