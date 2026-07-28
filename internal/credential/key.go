// Package credential 负责 HPRP 机器 Bearer Key 的签发、摘要存储和认证。
package credential

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/hprp"
)

const issueRandomBytes = 48

var (
	// ErrInvalidRecord 表示凭据身份、摘要或状态记录无效。
	ErrInvalidRecord = errors.New("HPRP 凭据记录无效")
	// ErrInvalidToken 表示 Bearer Key 的公开格式无效。
	ErrInvalidToken = errors.New("HPRP Bearer Key 格式无效")
	// ErrUnauthenticated 对外统一表示 Key 不存在、错误、过期或已吊销。
	ErrUnauthenticated = errors.New("HPRP 终端未认证")
)

var credentialIDPattern = regexp.MustCompile(`^cred-[a-z2-7]{26}$`)

const credentialIDLength = len("cred-") + 26

// Status 是持久化凭据的生命周期状态。
type Status string

const (
	StatusActive  Status = "active"
	StatusRevoked Status = "revoked"
)

// Record 是服务端持久化的机器凭据摘要，不包含可直接使用的 Secret。
type Record struct {
	CredentialID string     `json:"credential_id"`
	PrincipalID  string     `json:"principal_id"`
	MachineID    string     `json:"machine_id"`
	SecretSHA256 string     `json:"secret_sha256"`
	Status       Status     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Identity 是 Upgrade 认证成功后提供给 Server 连接状态机的可信身份。
type Identity struct {
	CredentialID string
	PrincipalID  string
	MachineID    string
}

// Issue 生成一把至少包含 256 位随机 Secret 的机器 Key 及其摘要记录。
func Issue(principalID, machineID string, now time.Time, random io.Reader) (string, Record, error) {
	if !validPrincipalID(principalID) || hprp.ValidateMachineID(machineID) != nil || random == nil {
		return "", Record{}, ErrInvalidRecord
	}
	randomData := make([]byte, issueRandomBytes)
	if _, err := io.ReadFull(random, randomData); err != nil {
		return "", Record{}, fmt.Errorf("%w: 读取安全随机数", ErrInvalidRecord)
	}
	credentialID := "cred-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(randomData[:16]))
	secret := base64.RawURLEncoding.EncodeToString(randomData[16:])
	digest := sha256.Sum256([]byte(secret))
	record := Record{
		CredentialID: credentialID,
		PrincipalID:  principalID,
		MachineID:    machineID,
		SecretSHA256: hex.EncodeToString(digest[:]),
		Status:       StatusActive,
		CreatedAt:    now.UTC(),
	}
	return "hpk_" + credentialID + "_" + secret, record, nil
}

// BearerCredentialID 解析 Key 中不敏感的 credential ID，供本地审计和服务端索引使用。
func BearerCredentialID(token string) (string, error) {
	credentialID, _, err := parseToken(token)
	return credentialID, err
}

// VerifyRecord 使用常量时间摘要比较验证一条凭据记录。
func VerifyRecord(record Record, token string, now time.Time) (Identity, error) {
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
	usable := record.Status == StatusActive && (record.ExpiresAt == nil || now.Before(*record.ExpiresAt))
	if !secretMatches || !usable {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{CredentialID: record.CredentialID, PrincipalID: record.PrincipalID, MachineID: record.MachineID}, nil
}

func parseToken(token string) (string, string, error) {
	if !strings.HasPrefix(token, "hpk_") {
		return "", "", ErrInvalidToken
	}
	remainder := strings.TrimPrefix(token, "hpk_")
	if len(remainder) <= credentialIDLength || remainder[credentialIDLength] != '_' {
		return "", "", ErrInvalidToken
	}
	credentialID := remainder[:credentialIDLength]
	secret := remainder[credentialIDLength+1:]
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if !credentialIDPattern.MatchString(credentialID) || err != nil || len(decoded) != 32 {
		return "", "", ErrInvalidToken
	}
	return credentialID, secret, nil
}

func validateRecord(record Record) error {
	if !credentialIDPattern.MatchString(record.CredentialID) || !validPrincipalID(record.PrincipalID) ||
		hprp.ValidateMachineID(record.MachineID) != nil || record.CreatedAt.IsZero() ||
		(record.Status != StatusActive && record.Status != StatusRevoked) {
		return ErrInvalidRecord
	}
	digest, err := hex.DecodeString(record.SecretSHA256)
	if err != nil || len(digest) != sha256.Size {
		return ErrInvalidRecord
	}
	return nil
}

func validPrincipalID(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && len(value) <= hprp.MaxLabelBytes
}
