package panel

import (
	"reflect"
	"testing"
)

func TestNormalizeRemovesTerminalControlSequences(t *testing.T) {
	input := "\x1b]0;窗口标题\a\x1b[31m红色\x1b[0m\n\x1b]8;;https://example.test\x1b\\链接\x1b]8;;\x1b\\"

	got := Normalize(input)
	want := []string{"红色", "链接"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeKeepsOnlyFinalCarriageReturnRedraw(t *testing.T) {
	input := "  start  \r  done  \r最终\r\nnext\r\n"

	got := Normalize(input)
	want := []string{"最终", "next"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizePreservesMeaningfulWhitespaceAndUnicode(t *testing.T) {
	input := "  缩进 😀  \n\n文本\x00\t \n\n\n"

	got := Normalize(input)
	want := []string{"  缩进 😀", "", "文本"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeReplacesInvalidUTF8(t *testing.T) {
	input := string([]byte{'a', 0xff, 'b'})

	got := Normalize(input)
	want := []string{"a�b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}
