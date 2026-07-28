package adminserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

const (
	defaultPageLimit  = 100
	maximumPageLimit  = 500
	pageTokenVersion  = 1
	maxPageTokenBytes = 1024
)

type pageCursor struct {
	Version int               `json:"version"`
	Method  adminproto.Method `json:"method"`
	Anchor  string            `json:"anchor"`
}

func normalizePageLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultPageLimit, nil
	}
	if limit < 0 || limit > maximumPageLimit {
		return 0, errors.New("分页 limit 超出范围")
	}
	return limit, nil
}

func encodeCredentialPageToken(method adminproto.Method, credentialID uint64) (string, error) {
	return encodePageToken(method, strconv.FormatUint(credentialID, 10))
}

func encodePageToken(method adminproto.Method, anchor string) (string, error) {
	encoded, err := json.Marshal(pageCursor{Version: pageTokenVersion, Method: method, Anchor: anchor})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeCredentialPageToken(token string, method adminproto.Method) (uint64, error) {
	anchor, err := decodePageToken(token, method)
	if err != nil || anchor == "" {
		return 0, err
	}
	credentialID, err := strconv.ParseUint(anchor, 10, 64)
	if err != nil || credentialID == 0 || strconv.FormatUint(credentialID, 10) != anchor {
		return 0, errors.New("分页 token 锚点无效")
	}
	return credentialID, nil
}

func decodePageToken(token string, method adminproto.Method) (string, error) {
	if token == "" {
		return "", nil
	}
	if len(token) > maxPageTokenBytes {
		return "", errors.New("分页 token 超限")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", errors.New("分页 token 编码无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor pageCursor
	if err := decoder.Decode(&cursor); err != nil {
		return "", errors.New("分页 token 结构无效")
	}
	if err := requirePageTokenEOF(decoder); err != nil || cursor.Version != pageTokenVersion || cursor.Method != method {
		return "", errors.New("分页 token 不适用于当前方法")
	}
	if cursor.Anchor == "" {
		return "", errors.New("分页 token 锚点无效")
	}
	return cursor.Anchor, nil
}

func requirePageTokenEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("分页 token 包含尾随内容")
	}
	return nil
}
