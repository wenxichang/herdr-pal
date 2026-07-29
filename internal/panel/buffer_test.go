package panel

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestBufferRefreshStoresNewestPageAndCopiesInput(t *testing.T) {
	input := numberedLines(130)
	var buffer Buffer

	buffer.Refresh("target-a", input)
	input[129] = "changed"
	got := buffer.Render()
	got[0] = "changed-render"

	if buffer.page != 0 || buffer.targetKey != "target-a" {
		t.Fatalf("Refresh state = page %d, target %q", buffer.page, buffer.targetKey)
	}
	if want := numberedLinesRange(30, 130); !reflect.DeepEqual(buffer.Render(), want) {
		t.Fatalf("Render() = %#v, want %#v", buffer.Render(), want)
	}
}

func TestBufferPagePositionUsesCurrentCachedPages(t *testing.T) {
	latest := numberedLinesRange(150, 180)
	history := numberedLinesRange(0, 150)
	var buffer Buffer
	buffer.Refresh("target-a", latest)
	if current, total := buffer.PagePosition(); current != 1 || total != 1 {
		t.Fatalf("PagePosition() = %d/%d, want 1/1", current, total)
	}

	snapshot := append(append([]string(nil), history...), latest...)
	if err := buffer.Expand("target-a", snapshot); err != nil {
		t.Fatal(err)
	}
	if current, total := buffer.PagePosition(); current != 2 || total != 3 {
		t.Fatalf("PagePosition() = %d/%d, want 2/3", current, total)
	}
	if err := buffer.PageDown(); err != nil {
		t.Fatal(err)
	}
	if current, total := buffer.PagePosition(); current != 1 || total != 3 {
		t.Fatalf("PagePosition() = %d/%d, want 1/3", current, total)
	}
}

func TestBufferPagesTextAndANSIWithSameAnchor(t *testing.T) {
	var buffer Buffer
	buffer.RefreshTerminal("session", numberedTerminalLinesRange(100, 200))
	if err := buffer.ExpandTerminal("session", numberedTerminalLinesRange(0, 200)); err != nil {
		t.Fatal(err)
	}
	page := buffer.RenderTerminal()
	if page.Current != 2 || page.Total != 2 || len(page.Lines) != PageSize {
		t.Fatalf("RenderTerminal() = %#v", page)
	}
	if page.Lines[0].Text != "line-0000" || page.Lines[0].ANSI != "\x1b[31mline-0000\x1b[0m" {
		t.Fatalf("first line = %#v", page.Lines[0])
	}
	page.Lines[0].Text = "changed"
	if buffer.RenderTerminal().Lines[0].Text != "line-0000" {
		t.Fatal("RenderTerminal() 暴露了内部缓存")
	}
}

func TestBufferNextReadSizeIncreasesToMaximum(t *testing.T) {
	var buffer Buffer
	buffer.Refresh("target-a", numberedLines(100))

	for page, want := 0, 200; page < 9; page, want = page+1, want+100 {
		buffer.page = page
		got, err := buffer.NextReadSize()
		if err != nil || got != want {
			t.Fatalf("page %d: NextReadSize() = %d, %v; want %d, nil", page, got, err, want)
		}
	}
	buffer.page = 9
	if _, err := buffer.NextReadSize(); !errors.Is(err, ErrOldestPage) {
		t.Fatalf("NextReadSize() error = %v, want ErrOldestPage", err)
	}
}

func TestBufferExpandUsesLastCompleteOverlapAndIgnoresNewerOutput(t *testing.T) {
	anchor := numberedLinesRange(200, 300)
	snapshot := append(numberedLinesRange(0, 100), anchor...)
	snapshot = append(snapshot, numberedLinesRange(100, 200)...)
	snapshot = append(snapshot, anchor...)
	snapshot = append(snapshot, "newer output")
	var buffer Buffer
	buffer.Refresh("target-a", anchor)

	if err := buffer.Expand("target-a", snapshot); err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if buffer.page != 1 {
		t.Fatalf("page = %d, want 1", buffer.page)
	}
	want := textLines(append(append([]string(nil), snapshot[:300]...), anchor...))
	if !reflect.DeepEqual(buffer.lines, want) {
		t.Fatalf("lines = %#v, want %#v", buffer.lines, want)
	}
}

func TestBufferExpandRejectsChangedTargetOrPanel(t *testing.T) {
	var buffer Buffer
	buffer.Refresh("target-a", numberedLines(100))

	if err := buffer.Expand("target-b", numberedLines(200)); !errors.Is(err, ErrPanelChanged) {
		t.Fatalf("target change error = %v, want ErrPanelChanged", err)
	}
	if buffer.targetKey != "" || buffer.page != 0 || len(buffer.lines) != 0 {
		t.Fatalf("target change did not reset buffer: %#v", buffer)
	}

	buffer.Refresh("target-a", numberedLines(100))
	if err := buffer.Expand("target-a", []string{"unrelated"}); !errors.Is(err, ErrPanelChanged) {
		t.Fatalf("panel change error = %v, want ErrPanelChanged", err)
	}
}

func TestBufferExpandStopsAtOldestKnownContentAndMaximum(t *testing.T) {
	var buffer Buffer
	lines := numberedLines(100)
	buffer.Refresh("target-a", lines)
	if err := buffer.Expand("target-a", lines); !errors.Is(err, ErrOldestPage) {
		t.Fatalf("no prefix error = %v, want ErrOldestPage", err)
	}
	if _, err := buffer.NextReadSize(); !errors.Is(err, ErrOldestPage) {
		t.Fatalf("known oldest NextReadSize error = %v, want ErrOldestPage", err)
	}

	buffer.Refresh("target-a", numberedLines(100))
	buffer.lines = textLines(numberedLines(MaxLines))
	buffer.newestLen = PageSize
	buffer.page = 9
	if err := buffer.Expand("target-a", numberedLines(MaxLines)); !errors.Is(err, ErrOldestPage) {
		t.Fatalf("maximum error = %v, want ErrOldestPage", err)
	}
}

func TestBufferRenderAllPagesAndPageDownUsesCache(t *testing.T) {
	var buffer Buffer
	buffer.targetKey = "target-a"
	buffer.lines = textLines(numberedLines(MaxLines))
	for page := 0; page < 10; page++ {
		buffer.page = page
		start := MaxLines - (page+1)*PageSize
		want := numberedLinesRange(start, start+PageSize)
		if got := buffer.Render(); !reflect.DeepEqual(got, want) {
			t.Fatalf("page %d: Render() = %#v, want %#v", page, got, want)
		}
	}

	buffer.page = 3
	if err := buffer.PageDown(); err != nil || buffer.page != 2 {
		t.Fatalf("PageDown() = %v, page %d; want nil, 2", err, buffer.page)
	}
	buffer.page = 0
	if err := buffer.PageDown(); !errors.Is(err, ErrNewestPage) {
		t.Fatalf("PageDown() error = %v, want ErrNewestPage", err)
	}
}

func TestBufferRenderKeepsShortLatestPageSeparateFromHistory(t *testing.T) {
	latest := numberedLinesRange(200, 230)
	history := numberedLinesRange(150, 200)
	var buffer Buffer
	buffer.Refresh("target-a", latest)

	if err := buffer.Expand("target-a", append(append([]string(nil), history...), latest...)); err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	got := buffer.Render()
	if !reflect.DeepEqual(got, history) {
		t.Fatalf("older page = %#v, want %#v", got, history)
	}
	got[0] = "changed-render"
	if !reflect.DeepEqual(buffer.Render(), history) {
		t.Fatalf("Render() exposed internal history slice")
	}
	if err := buffer.PageDown(); err != nil {
		t.Fatalf("PageDown() error = %v", err)
	}
	if got := buffer.Render(); !reflect.DeepEqual(got, latest) {
		t.Fatalf("latest page = %#v, want %#v", got, latest)
	}
}

func TestBufferShortLatestPagePaginatesLongAndMultipleHistory(t *testing.T) {
	latest := numberedLinesRange(200, 230)
	firstHistory := numberedLinesRange(50, 200)
	var buffer Buffer
	buffer.Refresh("target-a", latest)
	firstSnapshot := append(append([]string(nil), firstHistory...), latest...)
	if err := buffer.Expand("target-a", firstSnapshot); err != nil {
		t.Fatalf("first Expand() error = %v", err)
	}
	if got, want := buffer.Render(), numberedLinesRange(100, 200); !reflect.DeepEqual(got, want) {
		t.Fatalf("first older page = %#v, want %#v", got, want)
	}

	buffer.page = 2
	if got, want := buffer.Render(), numberedLinesRange(50, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial oldest page = %#v, want %#v", got, want)
	}

	buffer.page = 1
	olderHistory := numberedLinesRange(0, 50)
	secondSnapshot := append(append([]string(nil), olderHistory...), firstSnapshot...)
	if err := buffer.Expand("target-a", secondSnapshot); err != nil {
		t.Fatalf("second Expand() error = %v", err)
	}
	if got, want := buffer.Render(), numberedLinesRange(0, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("second older page = %#v, want %#v", got, want)
	}
	if err := buffer.PageDown(); err != nil {
		t.Fatalf("first PageDown() error = %v", err)
	}
	if got, want := buffer.Render(), numberedLinesRange(100, 200); !reflect.DeepEqual(got, want) {
		t.Fatalf("page down history = %#v, want %#v", got, want)
	}
	if err := buffer.PageDown(); err != nil {
		t.Fatalf("second PageDown() error = %v", err)
	}
	if got := buffer.Render(); !reflect.DeepEqual(got, latest) {
		t.Fatalf("page down latest = %#v, want %#v", got, latest)
	}
}

func numberedLines(count int) []string {
	return numberedLinesRange(0, count)
}

func numberedLinesRange(start, end int) []string {
	lines := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		lines = append(lines, fmt.Sprintf("line-%04d", index))
	}
	return lines
}

func numberedTerminalLinesRange(start, end int) []Line {
	lines := make([]Line, 0, end-start)
	for index := start; index < end; index++ {
		text := fmt.Sprintf("line-%04d", index)
		lines = append(lines, Line{Text: text, ANSI: "\x1b[31m" + text + "\x1b[0m"})
	}
	return lines
}
