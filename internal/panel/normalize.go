// Package panel 负责将 Herdr 终端快照转换为可安全展示的分页内容。
package panel

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Line 保存同一终端逻辑行的纯文本与安全 ANSI 表示。
type Line struct {
	Text string
	ANSI string
}

// Normalize 将终端近期快照转换为稳定的逻辑行。
//
// 它只移除终端控制序列和明显的重绘痕迹，不根据内容猜测或删除 Agent 语义。
func Normalize(text string) []string {
	paired := NormalizeANSI(text)
	lines := make([]string, len(paired))
	for index, line := range paired {
		lines[index] = line.Text
	}
	return lines
}

// NormalizeANSI 清理危险控制序列，并保留用于终端图片渲染的 SGR 样式。
func NormalizeANSI(text string) []Line {
	text = strings.ToValidUTF8(text, "�")
	text = sanitizeANSI(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")

	physicalLines := strings.Split(text, "\n")
	lines := make([]Line, 0, len(physicalLines))
	for _, physicalLine := range physicalLines {
		line := physicalLine
		if redraw := strings.LastIndexByte(line, '\r'); redraw >= 0 {
			line = "\x1b[0m" + line[redraw+1:]
		}
		line = collapseANSIHorizontalRules(line)
		line = trimANSIRightWhitespace(line)
		lines = append(lines, Line{Text: stripANSI(line), ANSI: line})
	}
	for len(lines) > 0 && lines[len(lines)-1].Text == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// JoinText 连接终端页的纯文本行。
func JoinText(lines []Line) string {
	joined := make([]string, len(lines))
	for index, line := range lines {
		joined[index] = line.Text
	}
	return strings.Join(joined, "\n")
}

// JoinANSI 连接终端页的安全 ANSI 行。
func JoinANSI(lines []Line) string {
	joined := make([]string, len(lines))
	for index, line := range lines {
		joined[index] = line.ANSI
	}
	return strings.Join(joined, "\n")
}

func collapseANSIHorizontalRules(text string) string {
	const maximum = 6
	var result strings.Builder
	result.Grow(len(text))
	run := 0
	for index := 0; index < len(text); {
		if end, ok := sgrSequenceEnd(text, index); ok {
			result.WriteString(text[index:end])
			index = end
			continue
		}
		current, size := utf8.DecodeRuneInString(text[index:])
		if current == '─' {
			run++
			if run <= maximum {
				result.WriteRune(current)
			}
			index += size
			continue
		}
		run = 0
		result.WriteRune(current)
		index += size
	}
	return result.String()
}

func stripANSI(text string) string {
	text = sanitizeANSI(text)
	var result strings.Builder
	result.Grow(len(text))
	for index := 0; index < len(text); {
		if end, ok := sgrSequenceEnd(text, index); ok {
			index = end
			continue
		}
		result.WriteByte(text[index])
		index++
	}
	return result.String()
}

func sanitizeANSI(text string) string {
	var result strings.Builder
	result.Grow(len(text))
	for index := 0; index < len(text); {
		if text[index] == 0 {
			index++
			continue
		}
		if text[index] != 0x1b {
			result.WriteByte(text[index])
			index++
			continue
		}
		if index+1 >= len(text) {
			index++
			continue
		}

		switch text[index+1] {
		case '[':
			start := index
			index += 2
			for index < len(text) {
				current := text[index]
				if current == '\n' || current == '\r' {
					break
				}
				index++
				if current >= 0x40 && current <= 0x7e {
					if current == 'm' {
						result.WriteString(text[start:index])
					}
					break
				}
			}
		case ']':
			index = skipControlString(text, index+2, true)
		case 'P', '_', '^', 'X':
			index = skipControlString(text, index+2, false)
		default:
			index = skipEscapeSequence(text, index+1)
		}
	}
	return result.String()
}

type ansiToken struct {
	raw     string
	visible bool
	space   bool
}

func trimANSIRightWhitespace(text string) string {
	tokens := make([]ansiToken, 0, len(text))
	lastVisible := -1
	for index := 0; index < len(text); {
		if end, ok := sgrSequenceEnd(text, index); ok {
			tokens = append(tokens, ansiToken{raw: text[index:end]})
			index = end
			continue
		}
		current, size := utf8.DecodeRuneInString(text[index:])
		tokens = append(tokens, ansiToken{raw: text[index : index+size], visible: true, space: unicode.IsSpace(current)})
		if !unicode.IsSpace(current) {
			lastVisible = len(tokens) - 1
		}
		index += size
	}
	if lastVisible < 0 {
		return ""
	}
	var result strings.Builder
	result.Grow(len(text))
	for index, token := range tokens {
		if token.visible && index > lastVisible && token.space {
			continue
		}
		result.WriteString(token.raw)
	}
	return result.String()
}

func sgrSequenceEnd(text string, index int) (int, bool) {
	if index+2 > len(text) || text[index] != 0x1b || text[index+1] != '[' {
		return index, false
	}
	for current := index + 2; current < len(text); current++ {
		value := text[current]
		if value < 0x40 || value > 0x7e {
			if value == '\n' || value == '\r' {
				return index, false
			}
			continue
		}
		if value != 'm' {
			return index, false
		}
		return current + 1, true
	}
	return index, false
}

func skipEscapeSequence(text string, index int) int {
	for index < len(text) && text[index] >= 0x20 && text[index] <= 0x2f {
		index++
	}
	if index >= len(text) || text[index] == '\n' || text[index] == '\r' {
		return index
	}
	if text[index] >= 0x30 && text[index] <= 0x7e {
		return index + 1
	}
	return index
}

func skipControlString(text string, index int, bellTerminates bool) int {
	firstNewline := -1
	for index < len(text) {
		if text[index] == '\n' && firstNewline < 0 {
			firstNewline = index
		}
		if bellTerminates && text[index] == 0x07 {
			return index + 1
		}
		if text[index] == 0x1b && index+1 < len(text) && text[index+1] == '\\' {
			return index + 2
		}
		if text[index] == 0xc2 && index+1 < len(text) && text[index+1] == 0x9c {
			return index + 2
		}
		index++
	}
	if firstNewline >= 0 {
		return firstNewline
	}
	return index
}
