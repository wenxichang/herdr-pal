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
	codeFenceOpen  = "\n```\n"
	codeFenceClose = "\n```"
)

// RenderPage 将终端行标记为“终端近期快照”，而非 assistant 回复。
//
// 返回内容是完整的 Markdown 逻辑页。调用方应将它原样交给 SplitMarkdown，以便分段后
// 仍保留目标、页码和独立闭合的代码块。
func RenderPage(target session.Target, page int, lines []string) string {
	name := target.DisplayAgent
	if name == "" {
		name = target.Agent
	}
	if name == "" {
		name = target.PaneID
	}
	header := fmt.Sprintf("终端近期快照\n目标：%s（%s）\n第 %d 页", safeLabel(name), safeLabel(target.PaneID), page)
	body := make([]string, len(lines))
	for index, line := range lines {
		body[index] = neutralizeFence(line)
	}
	return header + codeFenceOpen + strings.Join(body, "\n") + codeFenceClose
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
	if header, body, ok := renderedPageParts(content); ok {
		return splitRenderedPage(header, body, limit)
	}
	return splitPlain(content, limit)
}

func splitRenderedPage(header, body string, limit int) []string {
	if body == "" {
		part := fmt.Sprintf("%s\n分段 1/1%s%s", header, codeFenceOpen, codeFenceClose)
		if len(part) > limit {
			return nil
		}
		return []string{part}
	}

	maxDigits := len(strconv.Itoa(max(1, len(body))))
	for digits := 1; digits <= maxDigits; {
		markerReserve := "\n分段 " + strings.Repeat("9", digits) + "/" + strings.Repeat("9", digits)
		payloadLimit := limit - len(header) - len(markerReserve) - len(codeFenceOpen) - len(codeFenceClose)
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
		return wrapRenderedParts(header, parts)
	}
	return nil
}

func wrapRenderedParts(header string, parts []string) []string {
	result := make([]string, len(parts))
	for index, part := range parts {
		result[index] = fmt.Sprintf("%s\n分段 %d/%d%s%s%s", header, index+1, len(parts), codeFenceOpen, part, codeFenceClose)
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

func renderedPageParts(content string) (header, body string, ok bool) {
	open := strings.Index(content, codeFenceOpen)
	if open < 0 || !strings.HasPrefix(content, "终端近期快照\n") || !strings.HasSuffix(content, codeFenceClose) {
		return "", "", false
	}
	bodyStart := open + len(codeFenceOpen)
	bodyEnd := len(content) - len(codeFenceClose)
	return content[:open], content[bodyStart:bodyEnd], true
}

func safeLabel(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "", "```", "``\u200b`").Replace(value)
	return value
}

func neutralizeFence(value string) string {
	return strings.ReplaceAll(value, "```", "``\u200b`")
}
