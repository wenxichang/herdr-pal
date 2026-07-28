package credential

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
)

var (
	// ErrSourceRequired 表示凭据没有配置任何来源地址规则。
	ErrSourceRequired = errors.New("HPRP 凭据来源地址不能为空")
	// ErrSourceInvalid 表示来源地址规则格式、地址族或范围顺序无效。
	ErrSourceInvalid = errors.New("HPRP 凭据来源地址无效")
)

// SourceRule 是凭据文件中持久化的规范化来源地址规则。
//
// 字符串支持单 IP、CIDR 和闭区间三种格式。
type SourceRule string

type sourceKind uint8

const (
	sourceKindAddress sourceKind = iota
	sourceKindPrefix
	sourceKindRange
)

type parsedSourceRule struct {
	kind   sourceKind
	addr   netip.Addr
	prefix netip.Prefix
	start  netip.Addr
	end    netip.Addr
}

// ParseSourceRule 解析并规范化一条来源地址规则。
func ParseSourceRule(value string) (SourceRule, error) {
	parsed, err := parseSourceRule(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	switch parsed.kind {
	case sourceKindAddress:
		return SourceRule(parsed.addr.String()), nil
	case sourceKindPrefix:
		return SourceRule(parsed.prefix.String()), nil
	case sourceKindRange:
		return SourceRule(parsed.start.String() + "-" + parsed.end.String()), nil
	default:
		return "", ErrSourceInvalid
	}
}

// NormalizeSourceRules 规范化、去重并稳定排序来源地址规则。
func NormalizeSourceRules(values []string) ([]SourceRule, error) {
	rules := make([]SourceRule, 0, len(values))
	seen := make(map[SourceRule]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		rule, err := ParseSourceRule(value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[rule]; exists {
			continue
		}
		seen[rule] = struct{}{}
		rules = append(rules, rule)
	}
	if len(rules) == 0 {
		return nil, ErrSourceRequired
	}
	sort.Slice(rules, func(left, right int) bool {
		leftKind := classifySourceRule(rules[left])
		rightKind := classifySourceRule(rules[right])
		if leftKind != rightKind {
			return leftKind < rightKind
		}
		return rules[left] < rules[right]
	})
	return rules, nil
}

// MatchSource 报告来源地址是否被至少一条规则允许。
func MatchSource(rules []SourceRule, source netip.Addr) bool {
	if !source.IsValid() {
		return false
	}
	source = normalizeSourceAddr(source)
	for _, rule := range rules {
		parsed, err := parseSourceRule(string(rule))
		if err != nil {
			continue
		}
		switch parsed.kind {
		case sourceKindAddress:
			if source == parsed.addr {
				return true
			}
		case sourceKindPrefix:
			if parsed.prefix.Contains(source) {
				return true
			}
		case sourceKindRange:
			if source.BitLen() == parsed.start.BitLen() && source.Compare(parsed.start) >= 0 && source.Compare(parsed.end) <= 0 {
				return true
			}
		}
	}
	return false
}

func parseSourceRule(value string) (parsedSourceRule, error) {
	if value == "" {
		return parsedSourceRule{}, ErrSourceInvalid
	}
	if strings.Contains(value, "-") {
		return parseSourceRange(value)
	}
	if strings.Contains(value, "/") {
		return parseSourcePrefix(value)
	}
	address, err := parseSourceAddr(value)
	if err != nil {
		return parsedSourceRule{}, err
	}
	return parsedSourceRule{kind: sourceKindAddress, addr: address}, nil
}

func parseSourcePrefix(value string) (parsedSourceRule, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
	if err != nil || prefix.Addr().Zone() != "" {
		return parsedSourceRule{}, ErrSourceInvalid
	}
	address := prefix.Addr()
	bits := prefix.Bits()
	if address.Is4In6() {
		if bits < 96 {
			return parsedSourceRule{}, ErrSourceInvalid
		}
		address = address.Unmap()
		bits -= 96
	}
	prefix = netip.PrefixFrom(address, bits).Masked()
	return parsedSourceRule{kind: sourceKindPrefix, prefix: prefix}, nil
}

func parseSourceRange(value string) (parsedSourceRule, error) {
	if strings.Count(value, "-") != 1 {
		return parsedSourceRule{}, ErrSourceInvalid
	}
	startValue, endValue, found := strings.Cut(value, "-")
	if !found {
		return parsedSourceRule{}, ErrSourceInvalid
	}
	start, err := parseSourceAddr(strings.TrimSpace(startValue))
	if err != nil {
		return parsedSourceRule{}, err
	}
	end, err := parseSourceAddr(strings.TrimSpace(endValue))
	if err != nil {
		return parsedSourceRule{}, err
	}
	if start.BitLen() != end.BitLen() || start.Compare(end) > 0 {
		return parsedSourceRule{}, ErrSourceInvalid
	}
	return parsedSourceRule{kind: sourceKindRange, start: start, end: end}, nil
}

func parseSourceAddr(value string) (netip.Addr, error) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, ErrSourceInvalid
	}
	return normalizeSourceAddr(address), nil
}

func normalizeSourceAddr(address netip.Addr) netip.Addr {
	if address.Zone() != "" {
		address = address.WithZone("")
	}
	return address.Unmap()
}

func classifySourceRule(rule SourceRule) sourceKind {
	switch {
	case strings.Contains(string(rule), "-"):
		return sourceKindRange
	case strings.Contains(string(rule), "/"):
		return sourceKindPrefix
	default:
		return sourceKindAddress
	}
}
