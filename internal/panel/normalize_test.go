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

func TestNormalizeDropsUnsafeC0AndC1ControlsOutsideEscapeSequences(t *testing.T) {
	input := "a\a\b\v\f\u0085b\tc"

	got := panel.Normalize(input)
	want := []string{"ab\tc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeCollapsesLongHorizontalRules(t *testing.T) {
	input := "保留 ──────\n压缩 ────────────── 尾部\n分开 ─────── x ────────"

	got := panel.Normalize(input)
	want := []string{"保留 ──────", "压缩 ────── 尾部", "分开 ────── x ──────"}
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
	input := "before\x1bPunfinished\nvisible\nstart\x1b]unterminated\nnext"

	got := panel.Normalize(input)
	want := []string{"before", "visible", "start", "next"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeConsumesTerminatedControlStringsAcrossLines(t *testing.T) {
	input := "before\x1b]osc payload\nmore osc\x1b\\after\nnext\x1bPdcs payload\nmore dcs\u009cfinal"

	got := panel.Normalize(input)
	want := []string{"beforeafter", "nextfinal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeDoesNotLetUnterminatedCSICrossPhysicalBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "completed before newline",
			input: "\x1b[31mred\nvisible",
			want:  []string{"red", "visible"},
		},
		{
			name:  "newline before final",
			input: "\x1b[31\nvisible",
			want:  []string{"", "visible"},
		},
		{
			name:  "carriage return before final",
			input: "\x1b[31\rvisible",
			want:  []string{"visible"},
		},
		{
			name:  "end of input before final",
			input: "before\x1b[31",
			want:  []string{"before"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := panel.Normalize(test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Normalize() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNormalizeRemovesStandardEscapeSequencesConservatively(t *testing.T) {
	input := "\x1bcA\x1bDB\x1bMC\x1b7D\x1b8E\x1b(BF\nstart\x1b(\nvisible\nend\x1b("

	got := panel.Normalize(input)
	want := []string{"ABCDEF", "start", "visible", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Normalize() = %#v, want %#v", got, want)
	}
}

func TestNormalizeDropsBareTrailingEscapePrefix(t *testing.T) {
	for _, test := range []struct {
		name      string
		input     string
		want      []string
		wantEmpty bool
	}{
		{name: "after visible text", input: "visible\x1b", want: []string{"visible"}},
		{name: "only escape", input: "\x1b", wantEmpty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := panel.Normalize(test.input)
			if test.wantEmpty && len(got) == 0 {
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Normalize() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNormalizeANSIKeepsSGRAndRemovesUnsafeControls(t *testing.T) {
	input := "\x1b]0;secret\x07\x1b[31m红色\x1b[0m\x1b[2J\n\x1b[34m──────────\x1b[0m"

	got := panel.NormalizeANSI(input)
	want := []panel.Line{
		{Text: "红色", ANSI: "\x1b[31m红色\x1b[0m"},
		{Text: "──────", ANSI: "\x1b[34m──────\x1b[0m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeANSI() = %#v, want %#v", got, want)
	}
}

func TestNormalizeANSICarriageReturnResetsDiscardedStyle(t *testing.T) {
	got := panel.NormalizeANSI("\x1b[31m旧内容\r新内容\x1b[0m")
	want := []panel.Line{{Text: "新内容", ANSI: "\x1b[0m新内容\x1b[0m"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeANSI() = %#v, want %#v", got, want)
	}
}

func TestNormalizeANSIJoinFunctionsUsePairedLines(t *testing.T) {
	lines := []panel.Line{{Text: "one", ANSI: "\x1b[31mone\x1b[0m"}, {Text: "two", ANSI: "two"}}
	if got := panel.JoinText(lines); got != "one\ntwo" {
		t.Fatalf("JoinText() = %q", got)
	}
	if got := panel.JoinANSI(lines); got != "\x1b[31mone\x1b[0m\ntwo" {
		t.Fatalf("JoinANSI() = %q", got)
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

func TestBufferNextReadSizeRetainsCompleteCachedAnchor(t *testing.T) {
	for _, test := range []struct {
		name        string
		latestStart int
		historyFrom int
		historyTo   int
		wantSize    int
		wantPage    []string
	}{
		{
			name:        "full maximum cache",
			latestStart: 1000,
			historyFrom: 30,
			historyTo:   1000,
			wantSize:    panel.MaxLines,
			wantPage:    externalLines(800, 900),
		},
		{
			name:        "short cache still keeps anchor",
			latestStart: 200,
			historyFrom: 50,
			historyTo:   200,
			wantSize:    300,
			wantPage:    externalLines(50, 100),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			latest := externalLines(test.latestStart, test.latestStart+30)
			history := externalLines(test.historyFrom, test.historyTo)
			snapshot := append(append([]string(nil), history...), latest...)
			var buffer panel.Buffer
			buffer.Refresh("target-a", latest)
			if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
				t.Fatalf("first pageup error = %v", err)
			}

			readSize, err := buffer.NextReadSize()
			if err != nil {
				t.Fatalf("NextReadSize() error = %v", err)
			}
			if readSize != test.wantSize {
				t.Fatalf("NextReadSize() = %d, want %d", readSize, test.wantSize)
			}
			recentStart := len(snapshot) - readSize
			if recentStart < 0 {
				recentStart = 0
			}
			recent := snapshot[recentStart:]
			if err := buffer.Expand("target-a", recent); err != nil {
				t.Fatalf("Expand(recent) error = %v", err)
			}
			if got := buffer.Render(); !reflect.DeepEqual(got, test.wantPage) {
				t.Fatalf("cached next page = %#v, want %#v", got, test.wantPage)
			}
		})
	}
}

func TestBufferConsumesCachedPageAfterMaxWindowRollsForward(t *testing.T) {
	latest := externalLines(1000, 1030)
	history := externalLines(30, 1000)
	snapshot := append(append([]string(nil), history...), latest...)
	var buffer panel.Buffer
	buffer.Refresh("target-a", latest)
	if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
		t.Fatalf("first pageup error = %v", err)
	}

	readSize, err := buffer.NextReadSize()
	if err != nil || readSize != panel.MaxLines {
		t.Fatalf("NextReadSize() = %d, %v; want %d, nil", readSize, err, panel.MaxLines)
	}
	rolled := append(append([]string(nil), snapshot[1:]...), "new output")
	if err := buffer.Expand("target-a", rolled[len(rolled)-readSize:]); err != nil {
		t.Fatalf("Expand(rolled) error = %v", err)
	}
	if got, want := buffer.Render(), externalLines(800, 900); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached page after roll = %#v, want %#v", got, want)
	}
}

func TestBufferRejectsShortRollingOverlap(t *testing.T) {
	latest := externalLines(1000, 1030)
	history := externalLines(30, 1000)
	snapshot := append(append([]string(nil), history...), latest...)
	var buffer panel.Buffer
	buffer.Refresh("target-a", latest)
	if err := externalPageUp(&buffer, "target-a", snapshot); err != nil {
		t.Fatalf("first pageup error = %v", err)
	}

	rolled := append([]string(nil), snapshot[len(snapshot)-99:]...)
	for index := len(rolled); index < panel.MaxLines; index++ {
		rolled = append(rolled, fmt.Sprintf("unrelated-%04d", index))
	}
	if err := buffer.Expand("target-a", rolled); !errors.Is(err, panel.ErrPanelChanged) {
		t.Fatalf("Expand(short overlap) error = %v, want ErrPanelChanged", err)
	}
	if got := buffer.Render(); got != nil {
		t.Fatalf("buffer was not reset: %#v", got)
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
