package lokiquery

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildLogQLUsesOnlyEscapedControlledFilters(t *testing.T) {
	query, err := buildLogQL(Query{
		PrincipalID: `u"} |= "leak`,
		MachineID:   `HOME.*`,
		Keyword:     `Error[0-9]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{service_name="herdr-pal-server"}`,
		`herdr_pal_audit_schema_version="1"`,
		`herdr_pal_audit_principal_id="u\"} |= \"leak"`,
		`herdr_pal_audit_machine_id=~"(?i).*HOME\\.\\*.*"`,
		`|~ "(?i)Error\\[0-9\\]"`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query = %q, want %q", query, want)
		}
	}
}

func TestClientRejectsOversizedAuditFilters(t *testing.T) {
	client := &Client{httpClient: http.DefaultClient, now: time.Now}
	for _, query := range []Query{
		{PrincipalID: strings.Repeat("u", MaxPrincipalIDBytes+1)},
		{MachineID: strings.Repeat("m", MaxMachineIDBytes+1)},
		{Keyword: strings.Repeat("k", MaxKeywordBytes+1)},
		{Keyword: "line\nbreak"},
	} {
		if _, err := client.normalizeQuery(query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query %#v error = %v", query, err)
		}
	}
}

func TestClientQueryBuildsRangeRequestAndParsesStreams(t *testing.T) {
	oldest := time.Date(2026, 7, 29, 12, 0, 0, 2, time.UTC)
	newest := oldest.Add(2 * time.Nanosecond)
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/loki/api/v1/query_range" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		received = request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
			"status":"success",
			"data":{"resultType":"streams","result":[
				{"stream":{"herdr_pal_audit_event_name":"user.input","herdr_pal_audit_principal_id":"fallback-user","herdr_pal_audit_machine_id":"home"},"values":[
					["`+strconv.FormatInt(oldest.UnixNano(), 10)+`","old body"],
					["`+strconv.FormatInt(newest.UnixNano(), 10)+`","new body",{"event_name":"terminal.output","herdr_pal_audit_principal_id":"user-a","herdr_pal_audit_machine_id":"office","herdr_pal_audit_agent":"claude","herdr_pal_audit_pane_id":"w1:p2","herdr_pal_audit_session_id_hash":"session-hash","herdr_pal_audit_action":"read","herdr_pal_audit_outcome":"accepted"}]
				]}
			]}
		}`)
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, Now: func() time.Time { return newest.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	start := oldest.Add(-time.Hour)
	end := newest.Add(time.Hour)
	page, err := client.Query(context.Background(), Query{
		PrincipalID: "user-a", MachineID: "HOME", Keyword: "error", Start: start, End: end, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Get("direction") != "backward" || received.Get("start") != strconv.FormatInt(start.UnixNano(), 10) || received.Get("end") != strconv.FormatInt(end.UnixNano(), 10) || received.Get("limit") != "2" {
		t.Fatalf("query params = %v", received)
	}
	if len(page.Items) != 2 || page.Items[0].Timestamp != newest || page.Items[1].Timestamp != oldest {
		t.Fatalf("items = %#v", page.Items)
	}
	if page.Items[0].EventName != "terminal.output" || page.Items[0].PrincipalID != "user-a" || page.Items[0].MachineID != "office" || page.Items[0].Agent != "claude" || page.Items[0].PaneID != "w1:p2" || page.Items[0].Body != "new body" {
		t.Fatalf("structured item = %#v", page.Items[0])
	}
	if page.Items[1].PrincipalID != "fallback-user" || page.Items[1].MachineID != "home" || page.Items[1].Agent != "" || page.Items[1].Body != "old body" {
		t.Fatalf("fallback item = %#v", page.Items[1])
	}
	if page.NextCursor == "" {
		t.Fatal("next cursor is empty")
	}

	received = nil
	_, err = client.Query(context.Background(), Query{Start: start, End: end, Limit: 2, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := received.Get("end"); got != strconv.FormatInt(oldest.Add(-time.Nanosecond).UnixNano(), 10) {
		t.Fatalf("cursor end = %q", got)
	}
}

func TestClientQueryAppliesDefaultsAndRejectsInvalidInput(t *testing.T) {
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received = request.URL.Query()
		_, _ = io.WriteString(writer, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Query(context.Background(), Query{}); err != nil {
		t.Fatal(err)
	}
	if received.Get("start") != strconv.FormatInt(now.Add(-24*time.Hour).UnixNano(), 10) || received.Get("end") != strconv.FormatInt(now.UnixNano(), 10) || received.Get("limit") != "100" {
		t.Fatalf("defaults = %v", received)
	}

	invalid := []Query{
		{Start: now.Add(-32 * 24 * time.Hour), End: now},
		{Start: now, End: now.Add(-time.Second)},
		{Limit: 501},
		{Cursor: "invalid"},
	}
	for _, query := range invalid {
		if _, err := client.Query(context.Background(), query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query %#v error = %v", query, err)
		}
	}
}

func TestClientQueryReturnsStableErrorForLokiFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "non success status", handler: func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(writer, "sensitive upstream body")
		}},
		{name: "loki failure", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, `{"status":"error","error":"sensitive protocol body"}`)
		}},
		{name: "wrong result type", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
		}},
		{name: "invalid values", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["bad","line"]]}]}}`)
		}},
		{name: "response too large", handler: func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, strings.Repeat("x", maxResponseBytes+1))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Query(context.Background(), Query{})
			if !errors.Is(err, ErrQuery) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("error leaked body: %v", err)
			}
		})
	}
}

func TestClientQueryTimeoutReturnsStableError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL:    server.URL,
		HTTPClient: &http.Client{Timeout: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Query(context.Background(), Query{})
	if !errors.Is(err, ErrQuery) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	for _, value := range []string{"", "loki.local", "ftp://loki.local", "https://user@loki.local", "https://loki.local/path", "https://loki.local?q=1", "https://loki.local#x"} {
		if _, err := New(Config{BaseURL: value}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("base URL %q error = %v", value, err)
		}
	}
}
