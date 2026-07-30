package adminauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	automationTokenIDBytes = 8
	automationSecretBytes  = 32
)

// ErrInvalidAutomationToken 表示自动化 Token 随机源、格式或摘要记录无效。
var ErrInvalidAutomationToken = errors.New("管理员自动化 Token 无效")

// AutomationTokenRecord 是认证文件持久化的 Token 摘要记录，不包含可用 Secret。
type AutomationTokenRecord struct {
	TokenID      string    `json:"token_id"`
	SecretSHA256 string    `json:"secret_sha256"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GenerateAutomationToken 生成一次明文自动化 Token 和可持久化摘要记录。
func GenerateAutomationToken(random io.Reader, now time.Time) (string, AutomationTokenRecord, error) {
	if random == nil || now.IsZero() {
		return "", AutomationTokenRecord{}, ErrInvalidAutomationToken
	}
	randomData := make([]byte, automationTokenIDBytes+automationSecretBytes)
	if _, err := io.ReadFull(random, randomData); err != nil {
		return "", AutomationTokenRecord{}, fmt.Errorf("%w: 读取安全随机数", ErrInvalidAutomationToken)
	}
	tokenID := hex.EncodeToString(randomData[:automationTokenIDBytes])
	secret := base64.RawURLEncoding.EncodeToString(randomData[automationTokenIDBytes:])
	digest := sha256.Sum256([]byte(secret))
	now = now.UTC()
	return "hpa_" + tokenID + "_" + secret, AutomationTokenRecord{
		TokenID: tokenID, SecretSHA256: hex.EncodeToString(digest[:]), Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// VerifyAutomationToken 使用 Token ID 和 Secret 摘要验证明文 Token。
func VerifyAutomationToken(record AutomationTokenRecord, token string) bool {
	tokenID, secret, ok := parseAutomationToken(token)
	if !ok || tokenID != record.TokenID {
		return false
	}
	want, err := hex.DecodeString(record.SecretSHA256)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func parseAutomationToken(token string) (string, string, bool) {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[0] != "hpa" || len(parts[1]) != automationTokenIDBytes*2 {
		return "", "", false
	}
	decodedID, err := hex.DecodeString(parts[1])
	if err != nil || len(decodedID) != automationTokenIDBytes || parts[1] != strings.ToLower(parts[1]) {
		return "", "", false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(secret) != automationSecretBytes {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func validAutomationTokenRecord(record AutomationTokenRecord) bool {
	if len(record.TokenID) != automationTokenIDBytes*2 || record.TokenID != strings.ToLower(record.TokenID) {
		return false
	}
	if decoded, err := hex.DecodeString(record.TokenID); err != nil || len(decoded) != automationTokenIDBytes {
		return false
	}
	if decoded, err := hex.DecodeString(record.SecretSHA256); err != nil || len(decoded) != sha256.Size {
		return false
	}
	return !record.CreatedAt.IsZero() && !record.UpdatedAt.IsZero() && !record.UpdatedAt.Before(record.CreatedAt)
}
