package command

import (
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  Action
	}{
		{name: "list", input: "/ls", want: Action{Kind: KindList}},
		{name: "select shorthand", input: "/2", want: Action{Kind: KindSelect, Index: 2}},
		{name: "select shorthand with leading zero", input: "/01", want: Action{Kind: KindSelect, Index: 1}},
		{name: "content", input: "/con", want: Action{Kind: KindContent}},
		{name: "page up", input: "/pageup", want: Action{Kind: KindPageUp}},
		{name: "page down", input: "/pagedn", want: Action{Kind: KindPageDown}},
		{name: "help", input: "/help", want: Action{Kind: KindHelp}},
		{name: "key up", input: "/key up", want: Action{Kind: KindKey, Keys: []string{"up"}}},
		{name: "key down", input: "/key down", want: Action{Kind: KindKey, Keys: []string{"down"}}},
		{name: "key aliases", input: "/key dn sp", want: Action{Kind: KindKey, Keys: []string{"down", "space"}}},
		{name: "key mixed separators", input: "/key down,sp dn space,", want: Action{Kind: KindKey, Keys: []string{"down", "space", "down", "space"}}},
		{name: "key repeated separators", input: "/key down,,  ,space", want: Action{Kind: KindKey, Keys: []string{"down", "space"}}},
		{name: "enter alias", input: "/enter", want: Action{Kind: KindKey, Keys: []string{"enter"}}},
		{name: "enter", input: "/key enter", want: Action{Kind: KindKey, Keys: []string{"enter"}}},
		{name: "escape", input: "/key esc", want: Action{Kind: KindKey, Keys: []string{"esc"}}},
		{name: "space", input: "/key space", want: Action{Kind: KindKey, Keys: []string{"space"}}},
		{name: "uppercase character", input: "/key A", want: Action{Kind: KindKey, Keys: []string{"A"}}},
		{name: "lowercase character", input: "/key z", want: Action{Kind: KindKey, Keys: []string{"z"}}},
		{name: "digit character", input: "/key 7", want: Action{Kind: KindKey, Keys: []string{"7"}}},
		{name: "slash prompt", input: "/slash clear", want: Action{Kind: KindPrompt, Text: "/clear"}},
		{name: "slash prompt preserves inner spacing", input: " /slash   clear   now ", want: Action{Kind: KindPrompt, Text: "/clear   now"}},
		{name: "prompt preserves input", input: "  hello \nworld  ", want: Action{Kind: KindPrompt, Text: "  hello \nworld  "}},
		{name: "slash in prompt is not command", input: "please use /key", want: Action{Kind: KindPrompt, Text: "please use /key"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) returned an error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseInvalidCommands(t *testing.T) {
	t.Parallel()

	overflow := strconv.FormatUint(uint64(math.MaxInt)+1, 10)
	tests := []struct {
		name  string
		input string
		usage string
	}{
		{name: "empty", input: "", usage: "可用命令"},
		{name: "whitespace", input: " \t\n ", usage: "可用命令"},
		{name: "removed select alias", input: "/sel 2", usage: "可用命令"},
		{name: "select shorthand zero", input: "/0", usage: "/N"},
		{name: "select shorthand negative", input: "/-1", usage: "可用命令"},
		{name: "select shorthand overflow", input: "/" + overflow, usage: "/N"},
		{name: "key missing value", input: "/key", usage: "/key KEYS"},
		{name: "key unsupported special", input: "/key tab", usage: "/key KEYS"},
		{name: "key control sequence", input: "/key ctrl+c", usage: "/key KEYS"},
		{name: "key non ascii", input: "/key 中", usage: "/key KEYS"},
		{name: "key multiple characters", input: "/key aa", usage: "/key KEYS"},
		{name: "key uppercase special", input: "/key UP", usage: "/key KEYS"},
		{name: "key title case special", input: "/key Enter", usage: "/key KEYS"},
		{name: "key enter first in sequence", input: "/key enter down", usage: "/key KEYS"},
		{name: "key enter later in sequence", input: "/key down,enter", usage: "/key KEYS"},
		{name: "key sequence too long", input: "/key " + strings.Repeat("a ", 33), usage: "/key KEYS"},
		{name: "slash missing content", input: "/slash", usage: "/slash TEXT"},
		{name: "uppercase command", input: "/KEY up", usage: "可用命令"},
		{name: "uppercase list", input: "/LS", usage: "可用命令"},
		{name: "unknown command", input: "/unknown", usage: "可用命令"},
		{name: "unknown command after whitespace", input: "  /unknown", usage: "可用命令"},
		{name: "removed key up alias", input: "/keyup", usage: "可用命令"},
		{name: "removed key down alias", input: "/keydn", usage: "可用命令"},
		{name: "removed space alias", input: "/space", usage: "可用命令"},
		{name: "removed escape alias", input: "/esc", usage: "可用命令"},
		{name: "list with argument", input: "/ls x", usage: "/ls"},
		{name: "content with argument", input: "/con x", usage: "/con"},
		{name: "page up with argument", input: "/pageup x", usage: "/pageup"},
		{name: "page down with argument", input: "/pagedn x", usage: "/pagedn"},
		{name: "enter alias with argument", input: "/enter x", usage: "/enter"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.input)
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("Parse(%q) error = %v, want errors.Is(err, ErrInvalidCommand)", tt.input, err)
			}
			if !strings.Contains(err.Error(), "用法") {
				t.Fatalf("Parse(%q) error = %q, want usage guidance", tt.input, err)
			}
			if !strings.Contains(err.Error(), tt.usage) {
				t.Fatalf("Parse(%q) error = %q, want usage containing %q", tt.input, err, tt.usage)
			}
		})
	}
}

func TestParseAnswer(t *testing.T) {
	t.Parallel()

	numberList := func(start, end int) string {
		values := make([]string, 0, end-start+1)
		for value := start; value <= end; value++ {
			values = append(values, strconv.Itoa(value))
		}
		return strings.Join(values, ",")
	}
	indexRange := func(start, end int) []int {
		values := make([]int, 0, end-start+1)
		for value := start; value <= end; value++ {
			values = append(values, value)
		}
		return values
	}

	tests := []struct {
		name  string
		input string
		want  Action
	}{
		{name: "index list", input: "/answer 1,2,3", want: Action{Kind: KindAnswer, Indexes: []int{1, 2, 3}}},
		{name: "index with custom text", input: "/answer 5 自定义答案", want: Action{Kind: KindAnswer, Indexes: []int{5}, Text: "自定义答案"}},
		{name: "plain text", input: "/answer 纯文本答案", want: Action{Kind: KindAnswer, Text: "纯文本答案"}},
		{name: "mixed separators", input: "/answer 1, 2  3", want: Action{Kind: KindAnswer, Indexes: []int{1, 2, 3}}},
		{name: "deduplicate indexes", input: "/answer 2,2,1,2", want: Action{Kind: KindAnswer, Indexes: []int{2, 1}}},
		{name: "leading zeros", input: "/answer 01,002", want: Action{Kind: KindAnswer, Indexes: []int{1, 2}}},
		{name: "text preserves inner spacing", input: "/answer 1  自定义  答案", want: Action{Kind: KindAnswer, Indexes: []int{1}, Text: "自定义  答案"}},
		{name: "text starting with digits and letters", input: "/answer 1 2号选项", want: Action{Kind: KindAnswer, Indexes: []int{1}, Text: "2号选项"}},
		{name: "surrounding whitespace", input: "  /answer   3   完成  ", want: Action{Kind: KindAnswer, Indexes: []int{3}, Text: "完成"}},
		{name: "max 32 indexes", input: "/answer " + numberList(1, 32), want: Action{Kind: KindAnswer, Indexes: indexRange(1, 32)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) returned an error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}

	overflowIndex := strconv.FormatUint(uint64(math.MaxInt)+1, 10)
	invalid := []struct {
		name  string
		input string
	}{
		{name: "missing answer", input: "/answer"},
		{name: "zero index", input: "/answer 0"},
		{name: "separators only", input: "/answer , ,"},
		{name: "too many indexes", input: "/answer " + numberList(1, 33)},
		{name: "overflow index", input: "/answer " + overflowIndex},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.input)
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("Parse(%q) error = %v, want errors.Is(err, ErrInvalidCommand)", tt.input, err)
			}
			if !strings.Contains(err.Error(), "用法") {
				t.Fatalf("Parse(%q) error = %q, want usage guidance", tt.input, err)
			}
		})
	}
}

func TestParseNewSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  Action
	}{
		{name: "defaults", input: "/new-session", want: Action{Kind: KindNewSession, Title: "默认标题"}},
		{name: "title only", input: "/new-session 修复登录", want: Action{Kind: KindNewSession, Title: "修复登录"}},
		{name: "title and absolute directory", input: "/new-session 修复登录 /Users/me/work", want: Action{Kind: KindNewSession, Title: "修复登录", Directory: "/Users/me/work"}},
		{name: "absolute directory only", input: "/new-session /Users/me/work", want: Action{Kind: KindNewSession, Title: "默认标题", Directory: "/Users/me/work"}},
		{name: "home directory", input: "/new-session 修复登录 ~/work", want: Action{Kind: KindNewSession, Title: "修复登录", Directory: "~/work"}},
		{name: "bare home", input: "/new-session ~", want: Action{Kind: KindNewSession, Title: "默认标题", Directory: "~"}},
		{name: "relative directory", input: "/new-session 修复登录 ./work", want: Action{Kind: KindNewSession, Title: "修复登录", Directory: "./work"}},
		{name: "parent directory", input: "/new-session 修复登录 ../work", want: Action{Kind: KindNewSession, Title: "修复登录", Directory: "../work"}},
		{name: "plain relative word is title", input: "/new-session work", want: Action{Kind: KindNewSession, Title: "work"}},
		{name: "multi token title", input: "/new-session 修复 登录 问题", want: Action{Kind: KindNewSession, Title: "修复 登录 问题"}},
		{name: "normalize whitespace", input: "  /new-session   修复   登录  ", want: Action{Kind: KindNewSession, Title: "修复 登录"}},
		{name: "title truncated to 256 runes", input: "/new-session " + strings.Repeat("长", 300), want: Action{Kind: KindNewSession, Title: strings.Repeat("长", maxSessionTitleRunes)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q) returned an error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseUnknownSlashStillRejected(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"/answers", "/new-sessions", "/new_session", "/answerx 1", "/new-sessionx /tmp"} {
		_, err := Parse(input)
		if !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("Parse(%q) error = %v, want errors.Is(err, ErrInvalidCommand)", input, err)
		}
	}
}

func TestHelpTextDocumentsDSHCommands(t *testing.T) {
	t.Parallel()

	help := HelpText()
	for _, want := range []string{"/answer", "/new-session", "dsh"} {
		if !strings.Contains(help, want) {
			t.Fatalf("HelpText() = %q, want %q", help, want)
		}
	}
}

func TestHelpTextDocumentsSupportedCommands(t *testing.T) {
	t.Parallel()

	help := HelpText()
	for _, want := range []string{"/help", "/N", "/key", "dn", "sp", "/enter", "/slash", "等待 200ms"} {
		if !strings.Contains(help, want) {
			t.Fatalf("HelpText() = %q, want %q", help, want)
		}
	}
	for _, removed := range []string{"/sel", "/keyup", "/keydn", "/space", "/esc", "/mode"} {
		if strings.Contains(help, removed) {
			t.Fatalf("HelpText() = %q, must not document removed command %q", help, removed)
		}
	}
}

func TestParseDoesNotEchoUnknownCommand(t *testing.T) {
	t.Parallel()

	const input = "/secret-command-with-private-data"
	_, err := Parse(input)
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Parse(%q) error = %v, want errors.Is(err, ErrInvalidCommand)", input, err)
	}
	if strings.Contains(err.Error(), input) || strings.Contains(err.Error(), "secret-command-with-private-data") {
		t.Fatalf("Parse(%q) error = %q, must not echo the unknown command", input, err)
	}
}
