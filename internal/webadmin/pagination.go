package webadmin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

const (
	webPageVersion   = 1
	defaultPageLimit = 100
	maximumPageLimit = 500
	maxPageTokenSize = 1024
)

var errInvalidPagination = errors.New("Web 管理分页参数无效")

type webPageCursor struct {
	Version  int    `json:"version"`
	Resource string `json:"resource"`
	Anchor   string `json:"anchor"`
}

func parsePagination(values url.Values, resource string, allowed ...string) (int, string, error) {
	allowedKeys := map[string]struct{}{"limit": {}, "page_token": {}}
	for _, key := range allowed {
		allowedKeys[key] = struct{}{}
	}
	for key, entries := range values {
		if _, ok := allowedKeys[key]; !ok || len(entries) != 1 {
			return 0, "", errInvalidPagination
		}
	}
	limit := defaultPageLimit
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maximumPageLimit {
			return 0, "", errInvalidPagination
		}
		limit = parsed
	}
	anchor, err := decodeWebPageToken(values.Get("page_token"), resource)
	if err != nil {
		return 0, "", err
	}
	return limit, anchor, nil
}

func encodeWebPageToken(resource, anchor string) (string, error) {
	if strings.TrimSpace(resource) == "" || anchor == "" {
		return "", errInvalidPagination
	}
	encoded, err := json.Marshal(webPageCursor{Version: webPageVersion, Resource: resource, Anchor: anchor})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeWebPageToken(token, resource string) (string, error) {
	if token == "" {
		return "", nil
	}
	if len(token) > maxPageTokenSize {
		return "", errInvalidPagination
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(decoded) == 0 || len(decoded) > maxPageTokenSize {
		return "", errInvalidPagination
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor webPageCursor
	if err := decoder.Decode(&cursor); err != nil || requireWebPageEOF(decoder) != nil || cursor.Version != webPageVersion || cursor.Resource != resource || cursor.Anchor == "" {
		return "", errInvalidPagination
	}
	return cursor.Anchor, nil
}

func requireWebPageEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errInvalidPagination
	}
	return nil
}
