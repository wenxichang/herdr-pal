package adminauth

import (
	"net/netip"
	"testing"
	"time"
)

func TestLoginGuardLocksUsernameOrSourceAfterFifthFailure(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	guard := NewLoginGuard(func() time.Time { return now })
	firstIP := netip.MustParseAddr("192.0.2.1")
	secondIP := netip.MustParseAddr("192.0.2.2")
	for attempt := 0; attempt < 4; attempt++ {
		guard.Failure("admin", firstIP)
		if !guard.Allow("admin", firstIP) {
			t.Fatalf("locked before fifth failure at attempt %d", attempt+1)
		}
	}
	guard.Failure("admin", firstIP)
	if guard.Allow("admin", secondIP) {
		t.Fatal("username bucket did not lock across source IPs")
	}

	guard = NewLoginGuard(func() time.Time { return now })
	for attempt := 0; attempt < 5; attempt++ {
		guard.Failure("user-"+string(rune('a'+attempt)), firstIP)
	}
	if guard.Allow("another", firstIP) {
		t.Fatal("source IP bucket did not lock across usernames")
	}
}

func TestLoginGuardSuccessClearsBucketsAndLockExpires(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	guard := NewLoginGuard(func() time.Time { return now })
	address := netip.MustParseAddr("192.0.2.10")
	for attempt := 0; attempt < 4; attempt++ {
		guard.Failure("admin", address)
	}
	guard.Success("admin", address)
	for attempt := 0; attempt < 4; attempt++ {
		guard.Failure("admin", address)
	}
	if !guard.Allow("admin", address) {
		t.Fatal("successful login did not clear failure buckets")
	}
	guard.Failure("admin", address)
	if guard.Allow("admin", address) {
		t.Fatal("fifth post-success failure did not lock")
	}
	now = now.Add(15*time.Minute - time.Second)
	if guard.Allow("admin", address) {
		t.Fatal("lock expired early")
	}
	now = now.Add(time.Second)
	if !guard.Allow("admin", address) {
		t.Fatal("lock did not expire after 15 minutes")
	}
}

func TestLoginGuardKeepsDifferentUsersAndSourcesIndependent(t *testing.T) {
	guard := NewLoginGuard(time.Now)
	firstIP := netip.MustParseAddr("192.0.2.20")
	secondIP := netip.MustParseAddr("192.0.2.21")
	for attempt := 0; attempt < 5; attempt++ {
		guard.Failure("admin", firstIP)
	}
	if !guard.Allow("operator", secondIP) {
		t.Fatal("unrelated username and source were locked")
	}
	if guard.Allow("admin", netip.Addr{}) {
		t.Fatal("invalid source address was accepted")
	}
}
