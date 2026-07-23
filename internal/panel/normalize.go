// Package panel 负责将 Herdr 终端快照转换为可安全展示的分页内容。
package panel

import (
	"strings"
	"unicode"
)

// Normalize 将终端近期快照转换为稳定的逻辑行。
//
// 它只移除终端控制序列和明显的重绘痕迹，不根据内容猜测或删除 Agent 语义。
func Normalize(text string) []string {
	text = strings.ToValidUTF8(text, "�")
	text = stripANSI(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")

	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if redraw := strings.LastIndexByte(line, '\r'); redraw >= 0 {
			line = line[redraw+1:]
		}
		lines[index] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func stripANSI(text string) string {
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
			index += 2
			for index < len(text) {
				current := text[index]
				if current == '\n' || current == '\r' {
					break
				}
				index++
				if current >= 0x40 && current <= 0x7e {
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
