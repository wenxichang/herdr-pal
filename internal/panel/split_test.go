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
