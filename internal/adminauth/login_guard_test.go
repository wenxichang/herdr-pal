package adminauth

import (
	"fmt"
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
		if locked := failLoginAttempt(t, guard, "admin", firstIP); locked {
			t.Fatalf("locked before fifth failure at attempt %d", attempt+1)
		}
	}
	if locked := failLoginAttempt(t, guard, "admin", firstIP); !locked {
		t.Fatal("fifth failure did not lock username")
	}
	if guard.Begin("admin", secondIP) {
		t.Fatal("username bucket did not lock across source IPs")
	}

	guard = NewLoginGuard(func() time.Time { return now })
	for attempt := 0; attempt < 5; attempt++ {
		failLoginAttempt(t, guard, "user-"+string(rune('a'+attempt)), firstIP)
	}
	if guard.Begin("another", firstIP) {
		t.Fatal("source IP bucket did not lock across usernames")
	}
}

func TestLoginGuardSuccessClearsBucketsAndLockExpires(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	guard := NewLoginGuard(func() time.Time { return now })
	address := netip.MustParseAddr("192.0.2.10")
	for attempt := 0; attempt < 4; attempt++ {
		failLoginAttempt(t, guard, "admin", address)
	}
	if !guard.Begin("admin", address) {
		t.Fatal("successful attempt was not admitted")
	}
	guard.Finish("admin", address, true)
	for attempt := 0; attempt < 4; attempt++ {
		failLoginAttempt(t, guard, "admin", address)
	}
	if !guard.Begin("admin", address) {
		t.Fatal("successful login did not clear failure buckets")
	}
	if locked := guard.Finish("admin", address, false); !locked {
		t.Fatal("fifth post-success failure did not lock")
	}
	if guard.Begin("admin", address) {
		t.Fatal("fifth post-success failure did not lock")
	}
	now = now.Add(15*time.Minute - time.Second)
	if guard.Begin("admin", address) {
		t.Fatal("lock expired early")
	}
	now = now.Add(time.Second)
	if !guard.Begin("admin", address) {
		t.Fatal("lock did not expire after 15 minutes")
	}
	guard.Finish("admin", address, true)
}

func TestLoginGuardKeepsDifferentUsersAndSourcesIndependent(t *testing.T) {
	guard := NewLoginGuard(time.Now)
	firstIP := netip.MustParseAddr("192.0.2.20")
	secondIP := netip.MustParseAddr("192.0.2.21")
	for attempt := 0; attempt < 5; attempt++ {
		failLoginAttempt(t, guard, "admin", firstIP)
	}
	if !guard.Begin("operator", secondIP) {
		t.Fatal("unrelated username and source were locked")
	}
	guard.Finish("operator", secondIP, true)
	if guard.Begin("admin", netip.Addr{}) {
		t.Fatal("invalid source address was accepted")
	}
}

func TestLoginGuardRejectsConcurrentPasswordWorkForSameUsernameOrSource(t *testing.T) {
	guard := NewLoginGuard(time.Now)
	firstIP := netip.MustParseAddr("192.0.2.30")
	secondIP := netip.MustParseAddr("192.0.2.31")
	if !guard.Begin("admin", firstIP) {
		t.Fatal("first attempt was not admitted")
	}
	if guard.Begin("admin", secondIP) {
		t.Fatal("same username admitted concurrent password verification")
	}
	if guard.Begin("operator", firstIP) {
		t.Fatal("same source admitted concurrent password verification")
	}
	guard.Finish("admin", firstIP, false)
	if !guard.Begin("operator", firstIP) {
		t.Fatal("source remained in flight after attempt completion")
	}
	guard.Finish("operator", firstIP, true)
}

func TestLoginGuardBoundsArbitraryUsernameAndSourceState(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	guard := NewLoginGuard(func() time.Time { return now })
	for index := 0; index < loginBucketCapacity+128; index++ {
		address := netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", index+1))
		if guard.Begin(fmt.Sprintf("unknown-%d", index), address) {
			guard.Finish(fmt.Sprintf("unknown-%d", index), address, false)
		}
	}
	if len(guard.usernames) > loginBucketCapacity || len(guard.sources) > loginBucketCapacity {
		t.Fatalf("bucket sizes usernames=%d sources=%d", len(guard.usernames), len(guard.sources))
	}
}

func failLoginAttempt(t *testing.T, guard *LoginGuard, username string, source netip.Addr) bool {
	t.Helper()
	if !guard.Begin(username, source) {
		t.Fatalf("attempt for %q from %s was unexpectedly rejected", username, source)
	}
	return guard.Finish(username, source, false)
}
