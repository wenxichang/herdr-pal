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
		{name: "select", input: "  /sel   2 \t", want: Action{Kind: KindSelect, Index: 2}},
		{name: "select with leading zero", input: "/sel 01", want: Action{Kind: KindSelect, Index: 1}},
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
		{name: "select missing index", input: "/sel", usage: "/sel N"},
		{name: "select zero", input: "/sel 0", usage: "/sel N"},
		{name: "select negative", input: "/sel -1", usage: "/sel N"},
		{name: "select plus sign", input: "/sel +1", usage: "/sel N"},
		{name: "select full width digit", input: "/sel １", usage: "/sel N"},
		{name: "select multiple indices", input: "/sel 1 2", usage: "/sel N"},
		{name: "select overflow", input: "/sel " + overflow, usage: "/sel N"},
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

func TestHelpTextDocumentsSupportedCommands(t *testing.T) {
	t.Parallel()

	help := HelpText()
	for _, want := range []string{"/help", "/N", "/sel N", "/key", "dn", "sp", "/enter", "/slash"} {
		if !strings.Contains(help, want) {
			t.Fatalf("HelpText() = %q, want %q", help, want)
		}
	}
	for _, removed := range []string{"/keyup", "/keydn", "/space", "/esc"} {
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
