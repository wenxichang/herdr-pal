package adminauth

import (
	"errors"
	"testing"
	"time"
)

func TestSessionManagerEnforcesIdleExpiry(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	manager := newTestSessionManager(t, &now)
	created, err := manager.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Minute)
	if _, ok := manager.Get(created.ID); !ok {
		t.Fatal("session expired before idle timeout")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := manager.Get(created.ID); ok {
		t.Fatal("session survived idle timeout")
	}
}

func TestSessionManagerEnforcesAbsoluteExpiryDespiteActivity(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	manager := newTestSessionManager(t, &now)
	created, err := manager.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 24; step++ {
		now = now.Add(29 * time.Minute)
		if _, ok := manager.Get(created.ID); !ok {
			t.Fatalf("session expired before absolute timeout at %v", now)
		}
	}
	now = now.Add(24 * time.Minute)
	if _, ok := manager.Get(created.ID); ok {
		t.Fatal("session survived 12 hour absolute timeout")
	}
}

func TestSessionManagerRotatesIDAndCSRFAndRejectsMalformedValues(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	manager := newTestSessionManager(t, &now)
	created, err := manager.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	if !manager.VerifyCSRF(created.ID, created.CSRFToken) || manager.VerifyCSRF(created.ID, created.CSRFToken+"x") {
		t.Fatal("CSRF verification did not enforce exact token")
	}
	rotated, err := manager.Rotate(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ID == created.ID || rotated.CSRFToken == created.CSRFToken || rotated.Session.CreatedAt != created.Session.CreatedAt {
		t.Fatalf("rotated = %#v, created = %#v", rotated, created)
	}
	if _, ok := manager.Get(created.ID); ok || manager.VerifyCSRF(created.ID, created.CSRFToken) {
		t.Fatal("old session credentials remained valid")
	}
	if _, ok := manager.Get(rotated.ID); !ok || !manager.VerifyCSRF(rotated.ID, rotated.CSRFToken) {
		t.Fatal("rotated session credentials are invalid")
	}
	for _, value := range []string{"", "short", "not_base64!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		if _, ok := manager.Get(value); ok || manager.VerifyCSRF(rotated.ID, value) {
			t.Fatalf("manager accepted malformed value %q", value)
		}
	}
	if _, err := manager.Rotate("bad"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Rotate(malformed) error = %v", err)
	}
}

func TestSessionManagerRevokesUserSessions(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	manager := newTestSessionManager(t, &now)
	first, _ := manager.Create("admin")
	keep, _ := manager.Create("admin")
	other, _ := manager.Create("operator")
	manager.RevokeUserExcept("admin", keep.ID)
	if _, ok := manager.Get(first.ID); ok {
		t.Fatal("non-kept admin session survived")
	}
	if _, ok := manager.Get(keep.ID); !ok {
		t.Fatal("kept admin session was revoked")
	}
	if _, ok := manager.Get(other.ID); !ok {
		t.Fatal("other user session was revoked")
	}
	manager.RevokeUser("admin")
	if _, ok := manager.Get(keep.ID); ok {
		t.Fatal("RevokeUser left admin session active")
	}
	manager.Delete(other.ID)
	if _, ok := manager.Get(other.ID); ok {
		t.Fatal("Delete left session active")
	}
}

func newTestSessionManager(t *testing.T, now *time.Time) *SessionManager {
	t.Helper()
	manager, err := NewSessionManager(SessionConfig{
		Now:    func() time.Time { return *now },
		Random: &incrementingReader{next: 0x31},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
