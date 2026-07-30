package lokiquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultLimit     = 100
	maxLimit         = 500
	defaultRange     = 24 * time.Hour
	maxRange         = 31 * 24 * time.Hour
	requestTimeout   = 10 * time.Second
	maxResponseBytes = 16 * 1024 * 1024
)

var (
	// ErrInvalidConfig 表示 Loki 客户端配置不安全或不完整。
	ErrInvalidConfig = errors.New("Loki 查询配置无效")
	// ErrInvalidQuery 表示查询条件、时间范围或游标无效。
	ErrInvalidQuery = errors.New("Loki 查询条件无效")
	// ErrQuery 表示 Loki 不可达、响应过大或返回协议无效。
	ErrQuery = errors.New("Loki 审计查询失败")
)

// Config 指定 Loki 地址以及可替换的 HTTP 和时间依赖。
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Client 通过 Loki query_range API 执行受控审计查询。
type Client struct {
	endpoint   string
	httpClient *http.Client
	now        func() time.Time
}

type lokiResponse struct {
	Status string           `json:"status"`
	Data   lokiResponseData `json:"data"`
}

type lokiResponseData struct {
	ResultType string       `json:"resultType"`
	Result     []lokiStream `json:"result"`
}

type lokiStream struct {
	Stream map[string]string   `json:"stream"`
	Values [][]json.RawMessage `json:"values"`
}

// New 创建只允许访问固定 Loki query_range 路径的查询客户端。
func New(config Config) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalidConfig
	}
	parsed.Path = "/loki/api/v1/query_range"
	parsed.RawPath = ""

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	} else {
		cloned := *httpClient
		if cloned.Timeout <= 0 || cloned.Timeout > requestTimeout {
			cloned.Timeout = requestTimeout
		}
		httpClient = &cloned
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Client{endpoint: parsed.String(), httpClient: httpClient, now: now}, nil
}

// Query 查询审计日志并按时间倒序返回一页结果。
func (client *Client) Query(ctx context.Context, query Query) (Page, error) {
	if client == nil || client.httpClient == nil || client.now == nil {
		return Page{}, ErrInvalidConfig
	}
	normalized, err := client.normalizeQuery(query)
	if err != nil {
		return Page{}, err
	}
	logQL, err := buildLogQL(normalized)
	if err != nil {
		return Page{}, err
	}
	endpoint, err := url.Parse(client.endpoint)
	if err != nil {
		return Page{}, ErrInvalidConfig
	}
	values := endpoint.Query()
	values.Set("query", logQL)
	values.Set("direction", "backward")
	values.Set("start", strconv.FormatInt(normalized.Start.UnixNano(), 10))
	values.Set("end", strconv.FormatInt(normalized.End.UnixNano(), 10))
	values.Set("limit", strconv.Itoa(normalized.Limit))
	endpoint.RawQuery = values.Encode()

	requestContext, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Page{}, ErrQuery
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return Page{}, ErrQuery
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Page{}, ErrQuery
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return Page{}, ErrQuery
	}
	page, err := parseResponse(body, normalized.Limit)
	if err != nil {
		return Page{}, ErrQuery
	}
	return page, nil
}

func (client *Client) normalizeQuery(query Query) (Query, error) {
	if query.Limit == 0 {
		query.Limit = defaultLimit
	}
	if query.Limit < 1 || query.Limit > maxLimit {
		return Query{}, ErrInvalidQuery
	}
	if query.End.IsZero() {
		query.End = client.now().UTC()
	} else {
		query.End = query.End.UTC()
	}
	if query.Start.IsZero() {
		query.Start = query.End.Add(-defaultRange)
	} else {
		query.Start = query.Start.UTC()
	}
	if query.Cursor != "" {
		cursorEnd, err := decodeCursor(query.Cursor)
		if err != nil {
			return Query{}, ErrInvalidQuery
		}
		query.End = cursorEnd
	}
	if !query.Start.Before(query.End) || query.End.Sub(query.Start) > maxRange {
		return Query{}, ErrInvalidQuery
	}
	return query, nil
}

func buildLogQL(query Query) (string, error) {
	parts := []string{`{service_name="herdr-pal-server"}`}
	if query.PrincipalID != "" {
		parts = append(parts, `| herdr_pal_audit_principal_id=`+strconv.Quote(query.PrincipalID))
	}
	if query.MachineID != "" {
		pattern := `(?i).*` + regexp.QuoteMeta(query.MachineID) + `.*`
		parts = append(parts, `| herdr_pal_audit_machine_id=~`+strconv.Quote(pattern))
	}
	if query.Keyword != "" {
		pattern := `(?i)` + regexp.QuoteMeta(query.Keyword)
		parts = append(parts, `|~ `+strconv.Quote(pattern))
	}
	return strings.Join(parts, " "), nil
}

func parseResponse(body []byte, limit int) (Page, error) {
	var response lokiResponse
	if err := json.Unmarshal(body, &response); err != nil || response.Status != "success" || response.Data.ResultType != "streams" {
		return Page{}, ErrQuery
	}
	items := make([]Entry, 0, limit)
	for _, stream := range response.Data.Result {
		for _, rawValue := range stream.Values {
			item, err := parseValue(rawValue, stream.Stream)
			if err != nil {
				return Page{}, ErrQuery
			}
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(left, right int) bool {
		return items[left].Timestamp.After(items[right].Timestamp)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	page := Page{Items: items}
	if len(items) == limit {
		page.NextCursor = encodeCursor(items[len(items)-1].Timestamp.Add(-time.Nanosecond))
	}
	return page, nil
}

func parseValue(value []json.RawMessage, labels map[string]string) (Entry, error) {
	if len(value) != 2 && len(value) != 3 {
		return Entry{}, ErrQuery
	}
	var timestampText string
	var body string
	if err := json.Unmarshal(value[0], &timestampText); err != nil {
		return Entry{}, ErrQuery
	}
	if err := json.Unmarshal(value[1], &body); err != nil {
		return Entry{}, ErrQuery
	}
	timestampNS, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return Entry{}, ErrQuery
	}
	metadata := map[string]string(nil)
	if len(value) == 3 {
		if err := json.Unmarshal(value[2], &metadata); err != nil {
			return Entry{}, ErrQuery
		}
	}
	field := func(names ...string) string {
		for _, name := range names {
			if metadata != nil {
				if value, ok := metadata[name]; ok {
					return value
				}
			}
			if value, ok := labels[name]; ok {
				return value
			}
		}
		return ""
	}
	return Entry{
		Timestamp:     time.Unix(0, timestampNS).UTC(),
		EventName:     field("event_name", "herdr_pal_audit_event_name"),
		PrincipalID:   field("herdr_pal_audit_principal_id"),
		MachineID:     field("herdr_pal_audit_machine_id"),
		PaneID:        field("herdr_pal_audit_pane_id"),
		SessionIDHash: field("herdr_pal_audit_session_id_hash"),
		Action:        field("herdr_pal_audit_action"),
		Outcome:       field("herdr_pal_audit_outcome"),
		Body:          body,
	}, nil
}

func encodeCursor(value time.Time) string {
	encoded := strconv.FormatInt(value.UnixNano(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(encoded))
}

func decodeCursor(value string) (time.Time, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > 32 {
		return time.Time{}, ErrInvalidQuery
	}
	timestampNS, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil {
		return time.Time{}, ErrInvalidQuery
	}
	return time.Unix(0, timestampNS).UTC(), nil
}
