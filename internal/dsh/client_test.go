package dsh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientProtocol(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/v1/sessions":
			json.NewEncoder(w).Encode(map[string]any{"protocol": 1, "sessions": []Session{{ID: "s", Incarnation: "i", Status: "idle", Revision: "r"}}})
		case "/v1/prompt":
			calls++
			w.WriteHeader(409)
			w.Write([]byte(`{"error":"agent_busy"}`))
		case "/v1/read":
			w.Write([]byte(`{"text":"助手：完成"}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("x", 32))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := client.Sessions(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions=%v err=%v", sessions, err)
	}
	if err := client.Prompt(context.Background(), sessions[0], "key", "hello"); err == nil {
		t.Fatal("busy accepted")
	}
	if calls != 1 {
		t.Fatal("prompt retried")
	}
	text, err := client.Read(context.Background(), sessions[0])
	if err != nil || text != "助手：完成" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestClientRejectsRemoteAndRedirect(t *testing.T) {
	for _, endpoint := range []string{"http://example.com:1", "http://localhost:1", "https://127.0.0.1:1", "http://127.0.0.1:1/path", "http://user@127.0.0.1:1"} {
		if _, err := NewClient(endpoint, strings.Repeat("x", 32)); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed redirect") }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	client, _ := NewClient(redirect.URL, strings.Repeat("x", 32))
	if _, err := client.Sessions(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
}
