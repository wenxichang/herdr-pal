package audit

import (
	"regexp"
	"sort"
	"strings"
)

// RedactedValue 是审计正文中统一的凭据替换标记。
const RedactedValue = "[REDACTED]"

var (
	machineKeyPattern      = regexp.MustCompile(`\bhpk_[0-9]+_[A-Za-z0-9_-]{20,}\b`)
	automationTokenPattern = regexp.MustCompile(`\bhpa_[0-9a-f]{16}_[A-Za-z0-9_-]{20,}\b`)
	bearerPattern          = regexp.MustCompile(`(?i)(Authorization\s*:\s*Bearer\s+)[^\s\r\n]+`)
	cookiePattern          = regexp.MustCompile(`(?i)\b(Set-Cookie|Cookie)\s*:\s*[^\r\n]+`)
)

// Redactor 对审计正文执行有限、确定的凭据脱敏。
type Redactor struct {
	values []string
}

// NewRedactor 创建凭据脱敏器；values 通常包含 Bot Secret 和 OTLP Header 值。
func NewRedactor(values []string) *Redactor {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && value != RedactedValue {
			unique[value] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(unique))
	for value := range unique {
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(left, right int) bool { return len(ordered[left]) > len(ordered[right]) })
	return &Redactor{values: ordered}
}

// Redact 替换已知凭据、机器 Key、管理员自动化 Token 和常见认证 Header 值。
func (redactor *Redactor) Redact(content string) string {
	content = bearerPattern.ReplaceAllString(content, `${1}`+RedactedValue)
	content = cookiePattern.ReplaceAllStringFunc(content, func(header string) string {
		separator := strings.IndexByte(header, ':')
		if separator < 0 {
			return RedactedValue
		}
		return header[:separator+1] + " " + RedactedValue
	})
	content = machineKeyPattern.ReplaceAllString(content, RedactedValue)
	content = automationTokenPattern.ReplaceAllString(content, RedactedValue)
	if redactor == nil {
		return content
	}
	for _, value := range redactor.values {
		content = strings.ReplaceAll(content, value, RedactedValue)
	}
	return content
}
