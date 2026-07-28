// Package credential 负责 HPRP 机器 Bearer Key 的签发、摘要存储和认证。
package credential

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

const issueRandomBytes = 32

var (
	// ErrInvalidRecord 表示凭据身份、摘要、来源或状态记录无效。
	ErrInvalidRecord = errors.New("HPRP 凭据记录无效")
	// ErrInvalidToken 表示 Bearer Key 的公开格式无效。
	ErrInvalidToken = errors.New("HPRP Bearer Key 格式无效")
	// ErrUnauthenticated 对外统一表示 Key 不存在、错误、过期、禁用或来源不符。
	ErrUnauthenticated = errors.New("HPRP 终端未认证")
)

// Status 是持久化凭据的生命周期状态。
type Status string

const (
	StatusEnabled  Status = "enabled"
	StatusDisabled Status = "disabled"
)

// Record 是服务端持久化的机器凭据摘要，不包含可直接使用的 Secret。
type Record struct {
	CredentialID   uint64       `json:"credential_id"`
	PrincipalID    string       `json:"principal_id"`
	MachineID      string       `json:"machine_id"`
	SecretSHA256   string       `json:"secret_sha256"`
	Status         Status       `json:"status"`
	AllowedSources []SourceRule `json:"allowed_sources"`
	ExpiresAt      *time.Time   `json:"expires_at"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// Identity 是 Upgrade 认证成功后提供给 Server 连接状态机的可信身份。
type Identity struct {
	CredentialID uint64
	PrincipalID  string
	MachineID    string
}

// Issue 使用已分配的 credential ID 生成至少包含 256 位随机 Secret 的机器 Key 和摘要记录。
func Issue(credentialID uint64, principalID, machineID string, allowedSources []SourceRule, expiresAt *time.Time, now time.Time, random io.Reader) (string, Record, error) {
	now = now.UTC()
	if credentialID == 0 || !validPrincipalID(principalID) || hprp.ValidateMachineID(machineID) != nil || random == nil || now.IsZero() {
		return "", Record{}, ErrInvalidRecord
	}
	if err := validateSourceRules(allowedSources); err != nil {
		return "", Record{}, ErrInvalidRecord
	}
	var normalizedExpiry *time.Time
	if expiresAt != nil {
		value := expiresAt.UTC()
		if !value.After(now) {
			return "", Record{}, ErrInvalidRecord
		}
		normalizedExpiry = &value
	}
	randomData := make([]byte, issueRandomBytes)
	if _, err := io.ReadFull(random, randomData); err != nil {
		return "", Record{}, fmt.Errorf("%w: 读取安全随机数", ErrInvalidRecord)
	}
	secret := base64.RawURLEncoding.EncodeToString(randomData)
	digest := sha256.Sum256([]byte(secret))
	record := Record{
		CredentialID:   credentialID,
		PrincipalID:    principalID,
		MachineID:      machineID,
		SecretSHA256:   hex.EncodeToString(digest[:]),
		Status:         StatusEnabled,
		AllowedSources: append([]SourceRule(nil), allowedSources...),
		ExpiresAt:      normalizedExpiry,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	return "hpk_" + strconv.FormatUint(credentialID, 10) + "_" + secret, record, nil
}

// BearerCredentialID 解析 Key 中不敏感的十进制 credential ID，供本地审计和服务端索引使用。
func BearerCredentialID(token string) (uint64, error) {
	credentialID, _, err := parseToken(token)
	return credentialID, err
}

// VerifyRecord 使用常量时间摘要比较验证凭据、生命周期和真实来源地址。
func VerifyRecord(record Record, token string, now time.Time, source netip.Addr) (Identity, error) {
	credentialID, secret, err := parseToken(token)
	if err != nil || credentialID != record.CredentialID {
		return Identity{}, ErrUnauthenticated
	}
	if err := validateRecord(record); err != nil {
		return Identity{}, err
	}
	wantDigest, err := hex.DecodeString(record.SecretSHA256)
	if err != nil || len(wantDigest) != sha256.Size {
		return Identity{}, ErrInvalidRecord
	}
	gotDigest := sha256.Sum256([]byte(secret))
	secretMatches := subtle.ConstantTimeCompare(gotDigest[:], wantDigest) == 1
	usable := record.Status == StatusEnabled && (record.ExpiresAt == nil || now.Before(*record.ExpiresAt))
	if !secretMatches || !usable || !MatchSource(record.AllowedSources, source) {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{CredentialID: record.CredentialID, PrincipalID: record.PrincipalID, MachineID: record.MachineID}, nil
}

func parseToken(token string) (uint64, string, error) {
	if !strings.HasPrefix(token, "hpk_") {
		return 0, "", ErrInvalidToken
	}
	credentialText, secret, found := strings.Cut(strings.TrimPrefix(token, "hpk_"), "_")
	if !found || credentialText == "" || secret == "" {
		return 0, "", ErrInvalidToken
	}
	credentialID, err := strconv.ParseUint(credentialText, 10, 64)
	if err != nil || credentialID == 0 || strconv.FormatUint(credentialID, 10) != credentialText {
		return 0, "", ErrInvalidToken
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != issueRandomBytes {
		return 0, "", ErrInvalidToken
	}
	return credentialID, secret, nil
}

func validateRecord(record Record) error {
	if record.CredentialID == 0 || !validPrincipalID(record.PrincipalID) || hprp.ValidateMachineID(record.MachineID) != nil ||
		record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) ||
		(record.Status != StatusEnabled && record.Status != StatusDisabled) {
		return ErrInvalidRecord
	}
	if record.ExpiresAt != nil && !record.ExpiresAt.After(record.CreatedAt) {
		return ErrInvalidRecord
	}
	if err := validateSourceRules(record.AllowedSources); err != nil {
		return ErrInvalidRecord
	}
	digest, err := hex.DecodeString(record.SecretSHA256)
	if err != nil || len(digest) != sha256.Size {
		return ErrInvalidRecord
	}
	return nil
}

func validateSourceRules(rules []SourceRule) error {
	if len(rules) == 0 {
		return ErrSourceRequired
	}
	seen := make(map[SourceRule]struct{}, len(rules))
	for _, rule := range rules {
		normalized, err := ParseSourceRule(string(rule))
		if err != nil || normalized != rule {
			return ErrSourceInvalid
		}
		if _, exists := seen[rule]; exists {
			return ErrSourceInvalid
		}
		seen[rule] = struct{}{}
	}
	return nil
}

func validPrincipalID(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || len(value) > hprp.MaxLabelBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
