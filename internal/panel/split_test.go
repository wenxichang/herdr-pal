package panel

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestSplitMarkdownRespectsUTF8BoundariesAndPrefersNewlines(t *testing.T) {
	content := "第一行\n第二行\n😀😀😀"
	parts := SplitMarkdown(content, 10)
	if got := strings.Join(parts, ""); got != content {
		t.Fatalf("joined parts = %q, want %q", got, content)
	}
	for index, part := range parts {
		if len(part) > 10 || !utf8.ValidString(part) {
			t.Fatalf("part %d = %q (%d bytes, valid=%t)", index, part, len(part), utf8.ValidString(part))
		}
	}
	if parts[0] != "第一行\n" {
		t.Fatalf("first part = %q, want newline-preferred split", parts[0])
	}
}

func TestSplitMarkdownSplitsLongLineOnlyAtRuneBoundary(t *testing.T) {
	content := strings.Repeat("😀", 4)
	parts := SplitMarkdown(content, 5)
	if len(parts) != 4 {
		t.Fatalf("part count = %d, want 4", len(parts))
	}
	for index, part := range parts {
		if part != "😀" || !utf8.ValidString(part) || len(part) > 5 {
			t.Fatalf("part %d = %q", index, part)
		}
	}
}

func TestSplitMarkdownHasPredictableInvalidAndEmptyLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		limit   int
	}{
		{name: "empty", content: "", limit: 10},
		{name: "invalid", content: "text", limit: 0},
		{name: "too small for rune", content: "😀", limit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SplitMarkdown(test.content, test.limit); got != nil {
				t.Fatalf("SplitMarkdown(%q, %d) = %#v, want nil", test.content, test.limit, got)
			}
		})
	}
}

func TestRenderPageAndSplitMarkdownKeepTerminalPageSelfContained(t *testing.T) {
	target := session.Target{
		PaneID:       "workspace:tab:pane",
		DisplayAgent: "Codex",
		Title:        "任务",
	}
	content := RenderPage(target, 2, []string{
		"before ``` dangerous fence",
		strings.Repeat("中文😀终端内容 ", 40),
	})

	if !strings.Contains(content, "终端近期快照") || !strings.Contains(content, "Codex") || !strings.Contains(content, "第 2 页") {
		t.Fatalf("RenderPage() missing context: %q", content)
	}
	if strings.Contains(content, "before ``` dangerous") {
		t.Fatalf("RenderPage() did not neutralize terminal fence: %q", content)
	}

	const limit = 180
	parts := SplitMarkdown(content, limit)
	if len(parts) < 2 {
		t.Fatalf("SplitMarkdown() part count = %d, want multiple parts", len(parts))
	}
	for index, part := range parts {
		if len(part) > limit || !utf8.ValidString(part) {
			t.Fatalf("part %d exceeds limit or is invalid UTF-8", index)
		}
		if !strings.Contains(part, "终端近期快照") || !strings.Contains(part, "第 2 页") {
			t.Fatalf("part %d missing page context: %q", index, part)
		}
		marker := fmt.Sprintf("分段 %d/%d", index+1, len(parts))
		if !strings.Contains(part, marker) {
			t.Fatalf("part %d missing marker %q: %q", index, marker, part)
		}
		if strings.Count(part, "```") != 2 || !strings.HasSuffix(part, "\n```") {
			t.Fatalf("part %d does not have an independent closed code block: %q", index, part)
		}
	}
}

func TestSplitMarkdownAllowsEmptyTerminalPageAtExactWrapperLimit(t *testing.T) {
	content := RenderPage(session.Target{PaneID: "pane-1", DisplayAgent: "Codex"}, 0, nil)
	header, body, ok := renderedPageParts(content)
	if !ok || body != "" {
		t.Fatalf("RenderPage() = %q, want empty rendered terminal page", content)
	}
	limit := len(header) + len("\n分段 1/1") + len(codeFenceOpen) + len(codeFenceClose)

	parts := SplitMarkdown(content, limit)
	if len(parts) != 1 || len(parts[0]) != limit {
		t.Fatalf("SplitMarkdown() = %#v, want one %d-byte part", parts, limit)
	}
	if !strings.HasSuffix(parts[0], "\n```") {
		t.Fatalf("part does not close its code block: %q", parts[0])
	}
	if got := SplitMarkdown(content, limit-1); got != nil {
		t.Fatalf("SplitMarkdown(..., %d) = %#v, want nil", limit-1, got)
	}
}

func TestSplitMarkdownUsesActualOneDigitPartCountAtExactLimit(t *testing.T) {
	content := RenderPage(session.Target{PaneID: "pane-1", DisplayAgent: "Codex"}, 0, []string{"abcdefghij"})
	header, body, ok := renderedPageParts(content)
	if !ok {
		t.Fatalf("RenderPage() = %q, want rendered terminal page", content)
	}
	limit := len(header) + len("\n分段 1/5") + len(codeFenceOpen) + len(codeFenceClose) + 2

	parts := SplitMarkdown(content, limit)
	if len(parts) != 5 {
		t.Fatalf("part count = %d, want 5", len(parts))
	}
	if got := joinRenderedBodies(t, parts); got != body {
		t.Fatalf("joined body = %q, want %q", got, body)
	}
	for index, part := range parts {
		if len(part) > limit || !strings.Contains(part, fmt.Sprintf("分段 %d/5", index+1)) {
			t.Fatalf("part %d = %q", index, part)
		}
	}
}

func TestSplitMarkdownDoesNotSplitContentThatAlreadyFits(t *testing.T) {
	content := RenderPage(session.Target{PaneID: "pane-1", DisplayAgent: "Codex"}, 0, []string{
		"line-001",
		"line-002",
	})
	parts := SplitMarkdown(content, WeComContentLimit)
	if len(parts) != 1 {
		t.Fatalf("SplitMarkdown() part count = %d, want 1 for %d-byte content", len(parts), len(content))
	}
	if body := joinRenderedBodies(t, parts); body != "line-001\nline-002" {
		t.Fatalf("joined body = %q", body)
	}
}

func TestSplitMarkdownRecalculatesBudgetWhenPartCountReachesTen(t *testing.T) {
	content := RenderPage(session.Target{PaneID: "pane-1", DisplayAgent: "Codex"}, 0, []string{strings.Repeat("aaa\n", 10)})
	header, body, ok := renderedPageParts(content)
	if !ok {
		t.Fatalf("RenderPage() = %q, want rendered terminal page", content)
	}
	limit := len(header) + len("\n分段 99/99") + len(codeFenceOpen) + len(codeFenceClose) + 4

	parts := SplitMarkdown(content, limit)
	if len(parts) != 10 {
		t.Fatalf("part count = %d, want 10", len(parts))
	}
	if got := joinRenderedBodies(t, parts); got != body {
		t.Fatalf("joined body = %q, want %q", got, body)
	}
	for index, part := range parts {
		if len(part) > limit || !strings.Contains(part, fmt.Sprintf("分段 %d/10", index+1)) {
			t.Fatalf("part %d = %q", index, part)
		}
	}
}

func TestSplitMarkdownNonEmptyUnicodeRequiresRuneAndWrapperSpace(t *testing.T) {
	content := RenderPage(session.Target{PaneID: "pane-1", DisplayAgent: "Codex"}, 0, []string{"😀"})
	header, body, ok := renderedPageParts(content)
	if !ok {
		t.Fatalf("RenderPage() = %q, want rendered terminal page", content)
	}
	limit := len(header) + len("\n分段 1/1") + len(codeFenceOpen) + len(codeFenceClose) + len(body)

	parts := SplitMarkdown(content, limit)
	if len(parts) != 1 || len(parts[0]) != limit || joinRenderedBodies(t, parts) != body {
		t.Fatalf("SplitMarkdown() = %#v, want one exact UTF-8 part", parts)
	}
	if got := SplitMarkdown(content, limit-1); got != nil {
		t.Fatalf("SplitMarkdown(..., %d) = %#v, want nil", limit-1, got)
	}
}

func joinRenderedBodies(t *testing.T, parts []string) string {
	t.Helper()
	var body strings.Builder
	for index, part := range parts {
		_, fragment, ok := renderedPageParts(part)
		if !ok {
			t.Fatalf("part %d is not a rendered page: %q", index, part)
		}
		body.WriteString(fragment)
	}
	return body.String()
}
