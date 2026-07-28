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
	encoded, err := json.Marshal(pageCursor{Version: pageTokenVersion, Method: method, Anchor: strconv.FormatUint(credentialID, 10)})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeCredentialPageToken(token string, method adminproto.Method) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	if len(token) > maxPageTokenBytes {
		return 0, errors.New("分页 token 超限")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, errors.New("分页 token 编码无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var cursor pageCursor
	if err := decoder.Decode(&cursor); err != nil {
		return 0, errors.New("分页 token 结构无效")
	}
	if err := requirePageTokenEOF(decoder); err != nil || cursor.Version != pageTokenVersion || cursor.Method != method {
		return 0, errors.New("分页 token 不适用于当前方法")
	}
	credentialID, err := strconv.ParseUint(cursor.Anchor, 10, 64)
	if err != nil || credentialID == 0 || strconv.FormatUint(credentialID, 10) != cursor.Anchor {
		return 0, errors.New("分页 token 锚点无效")
	}
	return credentialID, nil
}

func requirePageTokenEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("分页 token 包含尾随内容")
	}
	return nil
}
