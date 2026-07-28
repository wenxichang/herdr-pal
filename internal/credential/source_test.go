package credential

import (
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"testing"
)

func TestParseSourceRuleNormalizesSupportedForms(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  SourceRule
	}{
		{name: "IPv4 地址", input: " 192.168.1.20 ", want: "192.168.1.20"},
		{name: "IPv6 地址", input: "2001:0db8::1", want: "2001:db8::1"},
		{name: "mapped IPv4 地址", input: "::ffff:192.168.1.20", want: "192.168.1.20"},
		{name: "IPv4 CIDR", input: "192.168.1.42/24", want: "192.168.1.0/24"},
		{name: "IPv6 CIDR", input: "2001:db8::1234/64", want: "2001:db8::/64"},
		{name: "mapped IPv4 CIDR", input: "::ffff:192.168.1.42/120", want: "192.168.1.0/24"},
		{name: "IPv4 范围", input: "192.168.1.1 - 192.168.1.5", want: "192.168.1.1-192.168.1.5"},
		{name: "IPv6 范围", input: "2001:db8::1-2001:db8::5", want: "2001:db8::1-2001:db8::5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseSourceRule(test.input)
			if err != nil {
				t.Fatalf("ParseSourceRule() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ParseSourceRule() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseSourceRuleRejectsInvalidForms(t *testing.T) {
	for _, input := range []string{
		"",
		"localhost",
		"192.168.1.1/33",
		"2001:db8::1/129",
		"192.168.1.5-192.168.1.1",
		"192.168.1.1-2001:db8::1",
		"192.168.1.1-",
		"-192.168.1.5",
		"192.168.1.1-192.168.1.2-192.168.1.3",
		"fe80::1%en0",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseSourceRule(input); !errors.Is(err, ErrSourceInvalid) {
				t.Fatalf("ParseSourceRule(%q) error = %v, want ErrSourceInvalid", input, err)
			}
		})
	}
}

func TestNormalizeSourceRulesSortsDeduplicatesAndKeepsOverlap(t *testing.T) {
	got, err := NormalizeSourceRules([]string{
		"192.168.1.1-192.168.1.5",
		"192.168.1.42/24",
		"192.168.1.1",
		"192.168.1.1",
		"2001:db8::1",
	})
	if err != nil {
		t.Fatalf("NormalizeSourceRules() error = %v", err)
	}
	want := []SourceRule{
		"192.168.1.1",
		"2001:db8::1",
		"192.168.1.0/24",
		"192.168.1.1-192.168.1.5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeSourceRules() = %#v, want %#v", got, want)
	}
}

func TestNormalizeSourceRulesRequiresAtLeastOneRule(t *testing.T) {
	for _, values := range [][]string{nil, {}, {"", " \t"}} {
		if _, err := NormalizeSourceRules(values); !errors.Is(err, ErrSourceRequired) {
			t.Fatalf("NormalizeSourceRules(%#v) error = %v, want ErrSourceRequired", values, err)
		}
	}
}

func TestMatchSourceSupportsAddressCIDRRangeAndBoundaries(t *testing.T) {
	rules, err := NormalizeSourceRules([]string{
		"192.168.1.20",
		"10.0.0.0/8",
		"172.16.0.10-172.16.0.12",
		"2001:db8::/64",
		"2001:db9::1-2001:db9::3",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		address string
		want    bool
	}{
		{address: "192.168.1.20", want: true},
		{address: "::ffff:192.168.1.20", want: true},
		{address: "10.255.255.255", want: true},
		{address: "11.0.0.1", want: false},
		{address: "172.16.0.10", want: true},
		{address: "172.16.0.12", want: true},
		{address: "172.16.0.13", want: false},
		{address: "2001:db8::ffff", want: true},
		{address: "2001:db9::1", want: true},
		{address: "2001:db9::3", want: true},
		{address: "2001:db9::4", want: false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			address := netip.MustParseAddr(test.address)
			if got := MatchSource(rules, address); got != test.want {
				t.Fatalf("MatchSource(%s) = %t, want %t", address, got, test.want)
			}
		})
	}
	if MatchSource(rules, netip.Addr{}) {
		t.Fatal("MatchSource() accepted invalid address")
	}
}

func TestSourceRulePersistsAsCanonicalJSONString(t *testing.T) {
	encoded, err := json.Marshal(struct {
		AllowedSources []SourceRule `json:"allowed_sources"`
	}{AllowedSources: []SourceRule{"192.168.1.0/24", "2001:db8::1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"allowed_sources":["192.168.1.0/24","2001:db8::1"]}`
	if string(encoded) != want {
		t.Fatalf("Marshal() = %s, want %s", encoded, want)
	}
}
