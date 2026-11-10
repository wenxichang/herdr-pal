// Package dsh 实现 dsh 原生 Remote API 客户端。
//
// 客户端只访问本机回环地址，使用 dsh 私有认证记录生成短期 Cookie；
// 认证材料仅短暂存在于内存，不写入日志或磁盘。
package dsh

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// maxCredentialBytes 限制认证文件读取大小，避免异常文件耗尽内存。
	maxCredentialBytes = 256 * 1024
	// credentialSecretBytes 是 dsh 当前唯一支持的 HMAC 密钥长度。
	credentialSecretBytes = 32
	// cookieLifetime 与 dsh 侧的短期 Cookie 约定一致。
	cookieLifetime = 5 * time.Minute
	// cookieVersion 是 Cookie value 的固定前缀；其他版本失败关闭。
	cookieVersion    = "v1"
	cookieNamePrefix = "dsh-auth-"
)

var (
	errCredentialFile    = errors.New("dsh 认证文件必须属于当前用户、权限受限且为普通文件")
	errCredentialSize    = errors.New("dsh 认证文件超过 256 KiB 限制")
	errCredentialFormat  = errors.New("dsh 认证记录格式不兼容")
	errCredentialVersion = errors.New("dsh 认证记录版本不兼容")
	errCredentialSecret  = errors.New("dsh 认证记录密钥无效")
)

// credentialsFile 只解析 dsh 当前固定格式的浏览器会话认证记录。
// 未知字段由 yaml 忽略，未知版本或类型直接失败关闭。
type credentialsFile struct {
	Version uint8 `yaml:"version"`
	Records struct {
		Session struct {
			Kind    string `yaml:"kind"`
			Payload struct {
				Version uint8  `yaml:"version"`
				Secret  string `yaml:"secret"`
			} `yaml:"payload"`
		} `yaml:"client-connection/browser-session"`
	} `yaml:"records"`
}

// cookiePayload 是 Cookie 中签名的 JSON 载荷，字段顺序固定以保证结果可复现。
type cookiePayload struct {
	Version   uint8  `json:"version"`
	Authority string `json:"authority"`
	IssuedAt  int64  `json:"issuedAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

// cookieForCredentials 读取 path 指向的 dsh 认证记录，并生成绑定 authority、
// 五分钟有效的 Cookie，返回 name=value 形式。
//
// authority 必须是 127.0.0.1:<port>；任何格式、权限或版本异常都会失败关闭。
// 返回的错误不包含认证材料。
func cookieForCredentials(path, authority string, now time.Time) (string, error) {
	if err := validateCookieAuthority(authority); err != nil {
		return "", err
	}
	secret, err := readCredentialSecret(path)
	if err != nil {
		return "", err
	}
	defer clear(secret)

	issuedAt := now.UnixMilli()
	payloadJSON, err := json.Marshal(cookiePayload{
		Version:   1,
		Authority: authority,
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt + cookieLifetime.Milliseconds(),
	})
	if err != nil {
		return "", errors.New("生成 dsh 认证载荷失败")
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	nameHash := sha256.Sum256([]byte(authority))
	name := cookieNamePrefix + base64.RawURLEncoding.EncodeToString(nameHash[:])
	return fmt.Sprintf("%s=%s.%s.%s", name, cookieVersion, encodedPayload, signature), nil
}

// validateCookieAuthority 只接受 IPv4 回环地址和合法端口，防止 Cookie 被
// 绑定到其他主机或伪造 authority 字符串。
func validateCookieAuthority(authority string) error {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host != "127.0.0.1" || port == "" {
		return errors.New("dsh authority 必须是 127.0.0.1:<port>")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return errors.New("dsh authority 端口必须是十进制数字")
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("dsh authority 端口必须在 1..65535")
	}
	return nil
}

// readCredentialSecret 打开并解析认证文件，返回 32 字节 HMAC 密钥。
// 调用方负责在使用后清除返回的切片。
func readCredentialSecret(path string) ([]byte, error) {
	file, err := openCredentialFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		clear(data)
		return nil, errors.New("读取 dsh 认证文件失败")
	}
	defer clear(data)
	if len(data) > maxCredentialBytes {
		return nil, errCredentialSize
	}

	var doc credentialsFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, errCredentialFormat
	}
	if doc.Version != 1 || doc.Records.Session.Payload.Version != 1 || doc.Records.Session.Kind != "grant" {
		return nil, errCredentialVersion
	}

	secret, err := base64.RawURLEncoding.Strict().DecodeString(doc.Records.Session.Payload.Secret)
	if err != nil {
		return nil, errCredentialSecret
	}
	if len(secret) != credentialSecretBytes {
		clear(secret)
		return nil, errCredentialSecret
	}
	return secret, nil
}
