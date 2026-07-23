package panel_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/panel"
)

func TestNormalizeRemovesTerminalControlSequences(t *testing.T) {
	input := "\x1b]0;窗口标题\a\x1b[31m红色\x1b[0m\n\x1b]8;;https://example.test\x1b\\链接\x1b]8;;\x1b\\"

	got := panel.Normalize(input)
	want := []string{"红色", "链接"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeKeepsOnlyFinalCarriageReturnRedraw(t *testing.T) {
	input := "  start  \r  done  \r最终\r\nnext\r\n"

	got := panel.Normalize(input)
	want := []string{"最终", "next"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizePreservesMeaningfulWhitespaceAndUnicode(t *testing.T) {
	input := "  缩进 😀  \n\n文本\x00\t \n\n\n"

	got := panel.Normalize(input)
	want := []string{"  缩进 😀", "", "文本"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeReplacesInvalidUTF8(t *testing.T) {
	input := string([]byte{'a', 0xff, 'b'})

	got := panel.Normalize(input)
	want := []string{"a�b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeStopsOSCAtC1StringTerminator(t *testing.T) {
	input := "\x1b]0;窗口标题\u009c终端可见内容"

	got := panel.Normalize(input)
	want := []string{"终端可见内容"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeRemovesSaveRestoreAndStringControls(t *testing.T) {
	input := "before\x1b7middle\x1b8after\x1bPdiscard\x1b\\kept\x1b_apc\u009ckept2\x1b^pm\x1b\\kept3\x1bXsos\x1b\\kept4"

	got := panel.Normalize(input)
	want := []string{"beforemiddleafterkeptkept2kept3kept4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeDoesNotConsumeFollowingLineForUnterminatedStringControl(t *testing.T) {
	input := "before\x1bPunfinished\nvisible"

	got := panel.Normalize(input)
	want := []string{"before", "visible"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestBufferPageUpConsumesAllCachedHistoricalPages(t *testing.T) {
	latest := externalLines(200, 230)
	history := externalLines(50, 200)
	var buffer panel.Buffer
	buffer.Refresh("target-a", latest)
	snapshot := append(append([]string(nil), history...), latest...)

	if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
		t.Fatalf("first pageup error = %v", err)
	}
	if got, want := buffer.Render(), externalLines(100, 200); !reflect.DeepEqual(got, want) {
		t.Fatalf("page 1 = %#v, want %#v", got, want)
	}
	if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
		t.Fatalf("second pageup error = %v", err)
	}
	if got, want := buffer.Render(), externalLines(50, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("page 2 = %#v, want %#v", got, want)
	}
	if err := externalPageUp(&buffer, "target-a", snapshot); !errors.Is(err, panel.ErrOldestPage) {
		t.Fatalf("third pageup error = %v, want ErrOldestPage", err)
	}
}

func TestBufferPageUpHandlesMultipleCachedPagesAndPageDown(t *testing.T) {
	latest := externalLines(250, 280)
	history := externalLines(0, 250)
	var buffer panel.Buffer
	buffer.Refresh("target-a", latest)
	snapshot := append(append([]string(nil), history...), latest...)

	for page, want := range [][]string{
		externalLines(150, 250),
		externalLines(50, 150),
		externalLines(0, 50),
	} {
		if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
			t.Fatalf("pageup %d error = %v", page+1, err)
		}
		if got := buffer.Render(); !reflect.DeepEqual(got, want) {
			t.Fatalf("page %d = %#v, want %#v", page+1, got, want)
		}
	}
	if err := buffer.PageDown(); err != nil {
		t.Fatalf("PageDown() error = %v", err)
	}
	if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
		t.Fatalf("pageup after PageDown error = %v", err)
	}
	if got, want := buffer.Render(), externalLines(0, 50); !reflect.DeepEqual(got, want) {
		t.Fatalf("pageup after PageDown = %#v, want %#v", got, want)
	}
}

func TestBufferPageUpMakesAllMaxLinesCachePagesReachable(t *testing.T) {
	latest := externalLines(1000, 1030)
	history := externalLines(30, 1000)
	var buffer panel.Buffer
	buffer.Refresh("target-a", latest)
	snapshot := append(append([]string(nil), history...), latest...)

	for page := 1; page <= 10; page++ {
		if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
			t.Fatalf("pageup %d error = %v", page, err)
		}
	}
	if got, want := buffer.Render(), externalLines(30, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("oldest max cache page = %#v, want %#v", got, want)
	}
	if _, err := buffer.NextReadSize(); !errors.Is(err, panel.ErrOldestPage) {
		t.Fatalf("NextReadSize() error = %v, want ErrOldestPage", err)
	}
}

func externalPageUp(buffer *panel.Buffer, targetKey string, snapshot []string) error {
	readSize, err := buffer.NextReadSize()
	if err != nil {
		return err
	}
	if readSize <= 0 {
		return fmt.Errorf("invalid read size: %d", readSize)
	}
	return buffer.Expand(targetKey, snapshot)
}

func externalLines(start, end int) []string {
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, fmt.Sprintf("line-%04d", index))
	}
	return lines
}
