package command

import (
	"errors"
	"math"
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
		{name: "content", input: "/con", want: Action{Kind: KindContent}},
		{name: "page up", input: "/pageup", want: Action{Kind: KindPageUp}},
		{name: "page down", input: "/pagedn", want: Action{Kind: KindPageDown}},
		{name: "key up alias", input: "/keyup", want: Action{Kind: KindKey, Key: "up"}},
		{name: "key up", input: "/key up", want: Action{Kind: KindKey, Key: "up"}},
		{name: "key down alias", input: "/keydn", want: Action{Kind: KindKey, Key: "down"}},
		{name: "key down", input: "/key down", want: Action{Kind: KindKey, Key: "down"}},
		{name: "enter alias", input: "/enter", want: Action{Kind: KindKey, Key: "enter"}},
		{name: "enter", input: "/key enter", want: Action{Kind: KindKey, Key: "enter"}},
		{name: "escape alias", input: "/esc", want: Action{Kind: KindKey, Key: "esc"}},
		{name: "escape", input: "/key esc", want: Action{Kind: KindKey, Key: "esc"}},
		{name: "space alias", input: "/space", want: Action{Kind: KindKey, Key: "space"}},
		{name: "space", input: "/key space", want: Action{Kind: KindKey, Key: "space"}},
		{name: "uppercase character", input: "/key A", want: Action{Kind: KindKey, Key: "A"}},
		{name: "lowercase character", input: "/key z", want: Action{Kind: KindKey, Key: "z"}},
		{name: "digit character", input: "/key 7", want: Action{Kind: KindKey, Key: "7"}},
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
			if got != tt.want {
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
		{name: "key missing value", input: "/key", usage: "/key up|down|enter|esc|space|X"},
		{name: "key special with extra argument", input: "/key up extra", usage: "/key up|down|enter|esc|space|X"},
		{name: "key character with extra argument", input: "/key A extra", usage: "/key up|down|enter|esc|space|X"},
		{name: "key unsupported special", input: "/key tab", usage: "/key up|down|enter|esc|space|X"},
		{name: "key control sequence", input: "/key ctrl+c", usage: "/key up|down|enter|esc|space|X"},
		{name: "key non ascii", input: "/key 中", usage: "/key up|down|enter|esc|space|X"},
		{name: "key multiple characters", input: "/key aa", usage: "/key up|down|enter|esc|space|X"},
		{name: "key uppercase special", input: "/key UP", usage: "/key up|down|enter|esc|space|X"},
		{name: "key title case special", input: "/key Enter", usage: "/key up|down|enter|esc|space|X"},
		{name: "uppercase command", input: "/KEY up", usage: "可用命令"},
		{name: "uppercase list", input: "/LS", usage: "可用命令"},
		{name: "unknown command", input: "/unknown", usage: "可用命令"},
		{name: "unknown command after whitespace", input: "  /unknown", usage: "可用命令"},
		{name: "key alias with argument", input: "/keyup x", usage: "/keyup"},
		{name: "list with argument", input: "/ls x", usage: "/ls"},
		{name: "content with argument", input: "/con x", usage: "/con"},
		{name: "page up with argument", input: "/pageup x", usage: "/pageup"},
		{name: "page down with argument", input: "/pagedn x", usage: "/pagedn"},
		{name: "key down alias with argument", input: "/keydn x", usage: "/keydn"},
		{name: "enter alias with argument", input: "/enter x", usage: "/enter"},
		{name: "escape alias with argument", input: "/esc x", usage: "/esc"},
		{name: "space alias with argument", input: "/space x", usage: "/space"},
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
