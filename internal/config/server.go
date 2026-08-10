package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/credential"
)

const maxAdminSocketPathBytes = 103

const maxRateLimit = 10000

const defaultWebAdminListen = "0.0.0.0:4001"

// OTLPLogsHeadersEnvName 是 OTLP Logs 请求头使用的标准环境变量名。
const OTLPLogsHeadersEnvName = "OTEL_EXPORTER_OTLP_LOGS_HEADERS"

// ServerConfig 是 herdr-pal-server 的完整配置。
type ServerConfig struct {
	WeCom     ServerWeComConfig  `json:"wecom"`
	Server    ListenerConfig     `json:"server"`
	Admin     AdminConfig        `json:"admin"`
	RateLimit RateLimitConfig    `json:"rate_limit"`
	Audit     AuditConfig        `json:"audit"`
	Log       LogConfig          `json:"log"`
	Files     ServerRuntimeFiles `json:"-"`
}

type serverConfigFile struct {
	WeCom     ServerWeComConfig `json:"wecom"`
	Server    ListenerConfig    `json:"server"`
	Admin     AdminConfig       `json:"admin"`
	RateLimit *RateLimitConfig  `json:"rate_limit"`
	Audit     AuditConfig       `json:"audit"`
	Log       LogConfig         `json:"log"`
}

// ServerWeComConfig 是服务端独占的企业微信机器人配置。
type ServerWeComConfig struct {
	BotID                string   `json:"bot_id"`
	Secret               string   `json:"secret"`
	RegistrationAdminIDs []string `json:"registration_admin_ids"`
}

// UnmarshalJSON 区分缺失字段与 null，并保持企业微信配置的严格字段校验。
func (config *ServerWeComConfig) UnmarshalJSON(data []byte) error {
	raw := struct {
		BotID                string          `json:"bot_id"`
		Secret               string          `json:"secret"`
		RegistrationAdminIDs json.RawMessage `json:"registration_admin_ids"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	config.BotID = raw.BotID
	config.Secret = raw.Secret
	config.RegistrationAdminIDs = nil
	if len(raw.RegistrationAdminIDs) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw.RegistrationAdminIDs), []byte("null")) {
		return errors.New("registration_admin_ids 必须是数组")
	}
	if err := json.Unmarshal(raw.RegistrationAdminIDs, &config.RegistrationAdminIDs); err != nil {
		return fmt.Errorf("registration_admin_ids 必须是字符串数组: %w", err)
	}
	return nil
}

// ListenerConfig 是 Relay WSS 监听和证书配置。
type ListenerConfig struct {
	Listen          string `json:"listen"`
	CertFile        string `json:"cert_file"`
	KeyFile         string `json:"key_file"`
	StateDir        string `json:"state_dir"`
	CredentialsFile string `json:"credentials_file"`
	AdminSocketPath string `json:"-"`
}

// AdminConfig 定义内嵌 HTTPS 管理台的监听地址和 Loki 查询地址。
type AdminConfig struct {
	Listen  string `json:"listen"`
	LokiURL string `json:"loki_url"`
}

// ServerRuntimeFiles 定义服务端固定使用的认证、引导和实时帮助文件。
type ServerRuntimeFiles struct {
	AuthFile      string
	BootstrapFile string
	HelpFile      string
}

// RateLimitConfig 定义单个企业微信用户的滚动窗口输入限额。
type RateLimitConfig struct {
	PerSecond int `json:"per_second"`
	PerMinute int `json:"per_minute"`
}

// UnmarshalJSON 保留显式零值，并为缺失字段应用稳定默认值。
func (config *RateLimitConfig) UnmarshalJSON(data []byte) error {
	raw := struct {
		PerSecond *int `json:"per_second"`
		PerMinute *int `json:"per_minute"`
	}{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	config.PerSecond = 1
	config.PerMinute = 20
	if raw.PerSecond != nil {
		config.PerSecond = *raw.PerSecond
	}
	if raw.PerMinute != nil {
		config.PerMinute = *raw.PerMinute
	}
	return nil
}

// AuditConfig 定义业务审计事件的输出方式。
type AuditConfig struct {
	Type       string            `json:"type"`
	Endpoint   string            `json:"endpoint"`
	SkipVerify bool              `json:"skip_verify"`
	Stderr     bool              `json:"stderr"`
	Headers    map[string]string `json:"-"`
}

// LoadServer 加载服务端配置；企业微信 Secret 只允许来自配置文件。
func LoadServer(path string, getenv func(string) string) (ServerConfig, error) {
	loaded, err := LoadServerAdmin(path)
	if err != nil {
		return ServerConfig{}, err
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if loaded.Audit.Type == "otlp" {
		headers, err := parseOTLPHeaders(getenv(OTLPLogsHeadersEnvName))
		if err != nil {
			return ServerConfig{}, fmt.Errorf("环境变量 %s 无效", OTLPLogsHeadersEnvName)
		}
		loaded.Audit.Headers = headers
	}
	if strings.TrimSpace(loaded.WeCom.BotID) == "" {
		return ServerConfig{}, fmt.Errorf("缺少必填字段 bot_id")
	}
	if strings.TrimSpace(loaded.WeCom.Secret) == "" {
		return ServerConfig{}, fmt.Errorf("缺少必填字段 wecom.secret")
	}
	if strings.TrimSpace(loaded.Server.Listen) == "" {
		return ServerConfig{}, fmt.Errorf("缺少必填字段 listen")
	}
	return loaded, nil
}

// LoadServerAdmin 加载不依赖企业微信 Secret 的服务端管理配置。
func LoadServerAdmin(path string) (ServerConfig, error) {
	raw, err := decodeFile[serverConfigFile](path)
	if err != nil {
		return ServerConfig{}, err
	}
	loaded := ServerConfig{
		WeCom: raw.WeCom, Server: raw.Server, Admin: raw.Admin, Audit: raw.Audit, Log: raw.Log,
		RateLimit: RateLimitConfig{PerSecond: 1, PerMinute: 20},
	}
	loaded.WeCom.RegistrationAdminIDs, err = normalizeRegistrationAdminIDs(loaded.WeCom.RegistrationAdminIDs)
	if err != nil {
		return ServerConfig{}, err
	}
	if raw.RateLimit != nil {
		loaded.RateLimit = *raw.RateLimit
	}
	if strings.TrimSpace(loaded.Server.StateDir) == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return ServerConfig{}, fmt.Errorf("获取用户配置目录: %w", err)
		}
		loaded.Server.StateDir = filepath.Join(configDir, "herdr-pal-server")
	}
	loaded.Server.StateDir = filepath.Clean(strings.TrimSpace(loaded.Server.StateDir))
	loaded.Server.AdminSocketPath, err = AdminSocketPath(loaded.Server.StateDir)
	if err != nil {
		return ServerConfig{}, err
	}
	if strings.TrimSpace(loaded.Server.CredentialsFile) == "" {
		loaded.Server.CredentialsFile = filepath.Join(loaded.Server.StateDir, "credentials.json")
	}
	loaded.Admin.Listen = strings.TrimSpace(loaded.Admin.Listen)
	if loaded.Admin.Listen == "" {
		loaded.Admin.Listen = defaultWebAdminListen
	}
	loaded.Admin.LokiURL = strings.TrimSpace(loaded.Admin.LokiURL)
	loaded.Files.AuthFile, err = DefaultServerAuthPath()
	if err != nil {
		return ServerConfig{}, err
	}
	loaded.Files.BootstrapFile, err = DefaultServerBootstrapPath()
	if err != nil {
		return ServerConfig{}, err
	}
	loaded.Files.HelpFile, err = DefaultServerHelpPath()
	if err != nil {
		return ServerConfig{}, err
	}
	certConfigured := strings.TrimSpace(loaded.Server.CertFile) != ""
	keyConfigured := strings.TrimSpace(loaded.Server.KeyFile) != ""
	if certConfigured != keyConfigured {
		return ServerConfig{}, fmt.Errorf("cert_file 与 key_file 必须同时配置")
	}
	if strings.TrimSpace(loaded.Log.Level) == "" {
		loaded.Log.Level = "info"
	}
	if err := validateRateLimit(loaded.RateLimit); err != nil {
		return ServerConfig{}, err
	}
	if err := validateAudit(&loaded.Audit); err != nil {
		return ServerConfig{}, err
	}
	if err := validateAdmin(loaded.Admin); err != nil {
		return ServerConfig{}, err
	}
	return loaded, nil
}

func normalizeRegistrationAdminIDs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		adminID := strings.TrimSpace(value)
		if adminID == "" {
			return nil, fmt.Errorf("wecom.registration_admin_ids 包含空白用户 ID")
		}
		if err := credential.ValidatePrincipalID(adminID); err != nil {
			return nil, fmt.Errorf("wecom.registration_admin_ids 包含无效用户 ID: %w", err)
		}
		if _, exists := seen[adminID]; exists {
			return nil, fmt.Errorf("wecom.registration_admin_ids 包含重复用户 ID: %s", adminID)
		}
		seen[adminID] = struct{}{}
		normalized = append(normalized, adminID)
	}
	return normalized, nil
}

func validateAdmin(config AdminConfig) error {
	_, port, err := net.SplitHostPort(config.Listen)
	if err != nil {
		return fmt.Errorf("admin.listen 必须是 host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("admin.listen 端口无效")
	}
	if config.LokiURL == "" {
		return nil
	}
	parsed, err := url.Parse(config.LokiURL)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("admin.loki_url 必须是绝对 HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("admin.loki_url 只支持 http 或 https")
	}
	if parsed.User != nil {
		return fmt.Errorf("admin.loki_url 不允许包含 userinfo")
	}
	if parsed.RawQuery != "" {
		return fmt.Errorf("admin.loki_url 不允许包含 query")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("admin.loki_url 不允许包含 fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("admin.loki_url 不允许包含 path")
	}
	return nil
}

func validateRateLimit(config RateLimitConfig) error {
	if config.PerSecond < 0 || config.PerSecond > maxRateLimit {
		return fmt.Errorf("rate_limit.per_second 必须在 0 到 %d 之间", maxRateLimit)
	}
	if config.PerMinute < 0 || config.PerMinute > maxRateLimit {
		return fmt.Errorf("rate_limit.per_minute 必须在 0 到 %d 之间", maxRateLimit)
	}
	return nil
}

func validateAudit(config *AuditConfig) error {
	config.Type = strings.ToLower(strings.TrimSpace(config.Type))
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	if config.Type == "" {
		config.Type = "none"
	}
	switch config.Type {
	case "none":
		if config.Endpoint != "" {
			return fmt.Errorf("audit.endpoint 只能在 audit.type=otlp 时配置")
		}
		if config.SkipVerify {
			return fmt.Errorf("audit.skip_verify 只能在 audit.type=otlp 且使用 HTTPS 时配置")
		}
		return nil
	case "otlp":
	default:
		return fmt.Errorf("audit.type 只支持 none 或 otlp")
	}
	if config.Endpoint == "" {
		return fmt.Errorf("audit.endpoint 在 audit.type=otlp 时必填")
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("audit.endpoint 必须是绝对 HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("audit.endpoint 只支持 http 或 https")
	}
	if parsed.User != nil {
		return fmt.Errorf("audit.endpoint 不允许包含 userinfo")
	}
	if parsed.RawQuery != "" {
		return fmt.Errorf("audit.endpoint 不允许包含 query")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("audit.endpoint 不允许包含 fragment")
	}
	if config.SkipVerify && parsed.Scheme != "https" {
		return fmt.Errorf("audit.skip_verify 只允许用于 HTTPS")
	}
	return nil
}

func parseOTLPHeaders(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, item := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("header 格式无效")
		}
		name, err := url.QueryUnescape(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("header 名称编码无效")
		}
		value, err := url.QueryUnescape(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("header 值编码无效")
		}
		canonicalName := textproto.CanonicalMIMEHeaderKey(name)
		if canonicalName == "" || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("header 名称或值无效")
		}
		if _, exists := headers[canonicalName]; exists {
			return nil, fmt.Errorf("header 重复")
		}
		headers[canonicalName] = value
	}
	return headers, nil
}

// AdminSocketPath 根据唯一的 state directory 推导本机 HPAP Socket 路径。
func AdminSocketPath(stateDir string) (string, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" || strings.ContainsRune(stateDir, '\x00') {
		return "", fmt.Errorf("state_dir 无效，无法推导 HPAP Admin Socket")
	}
	path := filepath.Join(filepath.Clean(stateDir), "admin.sock")
	if len(path) > maxAdminSocketPathBytes {
		return "", fmt.Errorf("派生的 admin.sock 路径过长（%d 字节，最多 %d 字节）", len(path), maxAdminSocketPathBytes)
	}
	return path, nil
}
