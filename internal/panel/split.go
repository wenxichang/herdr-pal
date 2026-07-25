package panel

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/session"
)

// WeComContentLimit 是企业微信 Markdown 单条消息的内容字节上限。
const WeComContentLimit = 20000

const (
	codeFenceOpen  = "```\n"
	codeFenceClose = "\n```"
	footerPrefix   = "[终端输出] "
)

// RenderPage 将终端行标记为“终端输出”，而非 assistant 回复。
//
// 返回内容是完整的 Markdown 逻辑页。调用方应将它原样交给 SplitMarkdown，以便分段后
// 仍保留目标、页码和独立闭合的代码块。
func RenderPage(target session.Target, page int, lines []string) string {
	current := max(1, page+1)
	return RenderPageWithTotal(target, current, current, lines)
}

// RenderPageWithTotal 将终端输出放在前面，并在末尾附加从 1 开始的缓存页码。
func RenderPageWithTotal(target session.Target, current, total int, lines []string) string {
	if current < 1 {
		current = 1
	}
	if total < current {
		total = current
	}
	title := target.Title
	if title == "" {
		title = "未命名"
	}
	agent := target.Agent
	if agent == "" {
		agent = target.DisplayAgent
	}
	if agent == "" {
		agent = target.PaneID
	}
	body := make([]string, len(lines))
	for index, line := range lines {
		body[index] = neutralizeFence(line)
	}
	footer := fmt.Sprintf("%s%s-%s(%s), 页码:[%d/%d]", footerPrefix, safeLabel(title), safeLabel(agent), safeLabel(target.PaneID), current, total)
	return renderPageParts(strings.Join(body, "\n"), footer)
}

// DecorateRenderedPage 为终端页补充 Relay 机器标识和本地序号。
func DecorateRenderedPage(content, machineID string, localIndex int) string {
	footer, body, ok := renderedPageParts(content)
	if !ok || machineID == "" || localIndex < 1 {
		return content
	}
	source := fmt.Sprintf("[%s/%d] ", safeLabel(machineID), localIndex)
	if strings.HasPrefix(strings.TrimPrefix(footer, footerPrefix), source) {
		return content
	}
	footer = strings.Replace(footer, footerPrefix, footerPrefix+source, 1)
	return renderPageParts(body, footer)
}

// AppendRenderedPageNote 在终端页脚后追加一行简短上下文。
func AppendRenderedPageNote(content, note string) string {
	if _, _, ok := renderedPageParts(content); !ok || strings.TrimSpace(note) == "" {
		return content
	}
	return content + "\n" + safeLabel(note)
}

// IsRenderedPage 报告内容是否是可安全重分段的终端页。
func IsRenderedPage(content string) bool {
	_, _, ok := renderedPageParts(content)
	return ok
}

// SplitMarkdown 按 UTF-8 字节上限切分企业微信 Markdown。
//
// 普通 Markdown 会在换行处优先切分。由 RenderPage 生成的终端页会被识别并重新包装，
// 使每一段都带有页码、分段序号和独立闭合的代码块。limit 非法或不足以容纳一个完整
// UTF-8 字符（或终端页包装）时返回 nil。
func SplitMarkdown(content string, limit int) []string {
	if content == "" || limit <= 0 {
		return nil
	}
	content = strings.ToValidUTF8(content, "�")
	if footer, body, ok := renderedPageParts(content); ok {
		return splitRenderedPage(footer, body, limit)
	}
	return splitPlain(content, limit)
}

func splitRenderedPage(footer, body string, limit int) []string {
	complete := renderPageParts(body, footer)
	if len(complete) <= limit {
		return []string{complete}
	}
	if body == "" {
		return nil
	}

	for digits := 1; digits <= len(strconv.Itoa(max(1, len(body)))); {
		markerReserve := "\n[分段] [" + strings.Repeat("9", digits) + "/" + strings.Repeat("9", digits) + "]"
		payloadLimit := limit - len(codeFenceOpen) - len(codeFenceClose) - len(markerReserve) - 1 - len(footer)
		if payloadLimit <= 0 {
			return nil
		}
		parts := splitPlain(body, payloadLimit)
		if len(parts) == 0 {
			return nil
		}
		requiredDigits := len(strconv.Itoa(len(parts)))
		if requiredDigits > digits {
			digits = requiredDigits
			continue
		}
		return wrapRenderedParts(footer, parts)
	}
	return nil
}

func wrapRenderedParts(footer string, parts []string) []string {
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = fmt.Sprintf("%s%s%s\n[分段] [%d/%d]\n%s", codeFenceOpen, part, codeFenceClose, index+1, len(parts), footer)
	}
	return result
}

func splitPlain(content string, limit int) []string {
	if content == "" || limit <= 0 {
		return nil
	}
	var parts []string
	for start := 0; start < len(content); {
		end := start + limit
		if end > len(content) {
			end = len(content)
		}
		for end > start && end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		if end == start {
			return nil
		}
		if end < len(content) {
			if newline := strings.LastIndexByte(content[start:end], '\n'); newline >= 0 {
				end = start + newline + 1
			}
		}
		parts = append(parts, content[start:end])
		start = end
	}
	return parts
}

func renderedPageParts(content string) (footer, body string, ok bool) {
	if !strings.HasPrefix(content, codeFenceOpen) {
		return "", "", false
	}
	footerStart := strings.LastIndex(content, "\n"+footerPrefix)
	if footerStart < 0 {
		return "", "", false
	}
	bodyEnd := strings.LastIndex(content[:footerStart], codeFenceClose)
	if bodyEnd < len(codeFenceOpen) {
		return "", "", false
	}
	footer = content[footerStart+1:]
	if !strings.HasPrefix(footer, footerPrefix) {
		return "", "", false
	}
	return footer, content[len(codeFenceOpen):bodyEnd], true
}

func renderPageParts(body, footer string) string {
	return codeFenceOpen + body + codeFenceClose + "\n" + footer
}

func safeLabel(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "", "```", "``\u200b`").Replace(value)
	return value
}

func neutralizeFence(value string) string {
	return strings.ReplaceAll(value, "```", "``\u200b`")
}
