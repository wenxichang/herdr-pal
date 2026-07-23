package policy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGuardAuthorizesOnlyConfiguredSingleUser(t *testing.T) {
	t.Parallel()

	guard, err := NewGuard("wang")
	if err != nil {
		t.Fatalf("NewGuard() error = %v", err)
	}
	if err := guard.Authorize(Identity{UserID: "wang", ChatType: "single"}); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}

	for _, identity := range []Identity{
		{UserID: "wang", ChatType: "group"},
		{UserID: "", ChatType: "single"},
		{UserID: "other", ChatType: "single"},
		{UserID: "Wang", ChatType: "single"},
		{UserID: " wang", ChatType: "single"},
		{UserID: "wang ", ChatType: "single"},
		{UserID: "wang", ChatType: "Single"},
	} {
		err := guard.Authorize(identity)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Authorize(%+v) error = %v, want ErrUnauthorized", identity, err)
		}
		if strings.Contains(err.Error(), identity.UserID) && identity.UserID != "" {
			t.Fatalf("Authorize(%+v) error leaked user id: %v", identity, err)
		}
	}
}

func TestNewGuardRejectsEmptyAllowedUser(t *testing.T) {
	t.Parallel()

	for _, allowedUserID := range []string{"", " \t\n"} {
		guard, err := NewGuard(allowedUserID)
		if guard != nil || !errors.Is(err, ErrInvalidAllowedUserID) {
			t.Fatalf("NewGuard(%q) = %v, %v, want nil and ErrInvalidAllowedUserID", allowedUserID, guard, err)
		}
	}
}

func TestGuardValidatesOnlyAllowedKeys(t *testing.T) {
	t.Parallel()

	guard, err := NewGuard("wang")
	if err != nil {
		t.Fatalf("NewGuard() error = %v", err)
	}

	for _, key := range []string{"up", "down", "enter", "esc", "space", "A", "z", "7"} {
		if err := guard.ValidateKey(key); err != nil {
			t.Fatalf("ValidateKey(%q) error = %v", key, err)
		}
	}
	for _, key := range []string{"", " ", "tab", "ctrl+c", "中", "aa", "!", " up", "up ", "UP", "Enter"} {
		err := guard.ValidateKey(key)
		if !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("ValidateKey(%q) error = %v, want ErrInvalidKey", key, err)
		}
		if strings.Contains(err.Error(), key) && key != "" {
			t.Fatalf("ValidateKey(%q) error leaked key: %v", key, err)
		}
	}
}

func TestKeyAuditContainsOnlyPermittedFields(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	audit, err := NewKeyAudit("wang", "pane-1", "sha256:abc", "A", at, "sent")
	if err != nil {
		t.Fatalf("NewKeyAudit() error = %v", err)
	}
	if audit.Key != "A" || audit.At != at || audit.Result != "sent" {
		t.Fatalf("NewKeyAudit() = %#v", audit)
	}
	if _, err := NewKeyAudit("wang", "pane-1", "sha256:abc", "ctrl+c", at, "rejected"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("NewKeyAudit() invalid key error = %v, want ErrInvalidKey", err)
	}

	typ := reflect.TypeOf(KeyAudit{})
	if typ.NumField() != 6 {
		t.Fatalf("KeyAudit has %d fields, want only 6 safe fields", typ.NumField())
	}
	for _, name := range []string{"UserID", "PaneID", "OccupantHash", "Key", "At", "Result"} {
		if _, ok := typ.FieldByName(name); !ok {
			t.Fatalf("KeyAudit missing permitted field %q", name)
		}
	}

	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(fields) != 6 {
		t.Fatalf("serialized audit fields = %#v, want only permitted fields", fields)
	}
	for _, name := range []string{"user_id", "pane_id", "occupant_hash", "key", "at", "result"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("serialized audit missing field %q: %#v", name, fields)
		}
	}
	for _, sensitive := range []string{"secret", "token", "prompt", "terminal", "content"} {
		if _, ok := fields[sensitive]; ok {
			t.Fatalf("serialized audit exposed %q: %#v", sensitive, fields)
		}
	}
}
