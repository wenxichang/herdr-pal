package webadmin

import (
	"net/url"
	"strings"
	"testing"
)

func TestPaginationDefaultsBoundsAndResourceIsolation(t *testing.T) {
	limit, anchor, err := parsePagination(url.Values{}, credentialPageResource)
	if err != nil || limit != defaultPageLimit || anchor != "" {
		t.Fatalf("default pagination = %d, %q, %v", limit, anchor, err)
	}
	token, err := encodeWebPageToken(credentialPageResource, "42")
	if err != nil {
		t.Fatal(err)
	}
	limit, anchor, err = parsePagination(url.Values{"limit": {"500"}, "page_token": {token}}, credentialPageResource)
	if err != nil || limit != 500 || anchor != "42" {
		t.Fatalf("pagination = %d, %q, %v", limit, anchor, err)
	}
	for _, values := range []url.Values{
		{"limit": {"0"}},
		{"limit": {"501"}},
		{"limit": {"1", "2"}},
		{"unknown": {"1"}},
		{"page_token": {token + "x"}},
	} {
		if _, _, err := parsePagination(values, credentialPageResource); err == nil {
			t.Fatalf("parsePagination accepted %#v", values)
		}
	}
	if _, _, err := parsePagination(url.Values{"page_token": {token}}, connectionPageResource); err == nil {
		t.Fatal("credential cursor was accepted by connection resource")
	}
	if _, _, err := parsePagination(url.Values{"page_token": {strings.Repeat("a", maxPageTokenSize+1)}}, credentialPageResource); err == nil {
		t.Fatal("oversized cursor was accepted")
	}
}
