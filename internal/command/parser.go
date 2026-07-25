// Package command 负责将企业微信文本解析为受限的桥接动作。
package command

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Kind 表示解析后的动作类别。
type Kind uint8

const (
	// KindList 请求列出已绑定的目标。
	KindList Kind = iota + 1
	// KindSelect 请求选择指定序号的目标。
	KindSelect
	// KindContent 请求读取当前目标内容。
	KindContent
	// KindPageUp 请求向上翻页。
	KindPageUp
	// KindPageDown 请求向下翻页。
	KindPageDown
	// KindKey 请求发送受限按键。
	KindKey
	// KindHelp 请求显示输入帮助。
	KindHelp
	// KindPrompt 请求发送普通文本提示。
	KindPrompt
)

// Action 是输入文本对应的桥接动作。
type Action struct {
	Kind  Kind
	Index int
	Keys  []string
	Text  string
}

// ErrInvalidCommand 表示输入为空或命令格式不受支持。
var ErrInvalidCommand = errors.New("无效命令")

const generalUsage = "用法: 可用命令见 /help"

const keyUsage = "/key 用法: /key KEYS"

const maxKeySequence = 32

const helpText = `输入帮助：
/ls                 列出 Agent
/N 或 /sel N        选择第 N 个 Agent，并显示最新 100 行
/help               显示本帮助
/con                显示最新 100 行并重置分页
/pageup、/pagedn    上翻、下翻缓存
/key KEYS           发送按键；逗号或空白分隔，最多 32 个
/enter              等同 /key enter
/slash TEXT         将 /TEXT 作为普通消息发送给 Agent

按键支持 up、down、esc、space、单个 ASCII 字母或数字；dn 等同 down，sp 等同 space。
相邻按键间隔 100ms。enter 只能单独发送，不能出现在多键队列中。
其他不以 / 开头的文本会直接发送给当前 Agent。`

// HelpText 返回当前支持的聊天输入帮助。
func HelpText() string {
	return helpText
}

// Parse 将输入文本解析为受限动作。非命令文本会保留原始内容作为提示。
func Parse(input string) (Action, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return Action{}, invalidCommand(generalUsage)
	}
	if !strings.HasPrefix(trimmed, "/") {
		return Action{Kind: KindPrompt, Text: input}, nil
	}

	fields := strings.Fields(trimmed)
	command := fields[0]
	if len(fields) == 1 && len(command) > 1 && isASCIIUnsignedInteger(command[1:]) {
		return parseSelectIndex(command[1:], "/N 用法: /N")
	}
	switch command {
	case "/ls":
		return parseAlias(fields, command, Action{Kind: KindList})
	case "/con":
		return parseAlias(fields, command, Action{Kind: KindContent})
	case "/pageup":
		return parseAlias(fields, command, Action{Kind: KindPageUp})
	case "/pagedn":
		return parseAlias(fields, command, Action{Kind: KindPageDown})
	case "/help":
		return parseAlias(fields, command, Action{Kind: KindHelp})
	case "/enter":
		return parseAlias(fields, command, Action{Kind: KindKey, Keys: []string{"enter"}})
	case "/sel":
		return parseSelect(fields)
	case "/key":
		return parseKeys(strings.TrimSpace(strings.TrimPrefix(trimmed, "/key")))
	case "/slash":
		return parseSlash(trimmed)
	default:
		return Action{}, invalidCommand(generalUsage)
	}
}

func parseAlias(fields []string, command string, action Action) (Action, error) {
	if len(fields) != 1 {
		return Action{}, invalidCommand(command + " 用法: " + command)
	}
	return action, nil
}

func parseSelect(fields []string) (Action, error) {
	if len(fields) != 2 || !isASCIIUnsignedInteger(fieldsAt(fields, 1)) {
		return Action{}, invalidCommand("/sel 用法: /sel N")
	}
	return parseSelectIndex(fields[1], "/sel 用法: /sel N")
}

func parseSelectIndex(value, usage string) (Action, error) {
	index, err := strconv.Atoi(value)
	if err != nil || index <= 0 {
		return Action{}, invalidCommand(usage)
	}
	return Action{Kind: KindSelect, Index: index}, nil
}

func parseKeys(raw string) (Action, error) {
	values := strings.FieldsFunc(raw, func(value rune) bool {
		return value == ',' || unicode.IsSpace(value)
	})
	if len(values) == 0 || len(values) > maxKeySequence {
		return Action{}, invalidCommand(keyUsage)
	}
	keys := make([]string, len(values))
	for index, value := range values {
		keys[index] = normalizeKey(value)
		if !isAllowedKey(keys[index]) {
			return Action{}, invalidCommand(keyUsage)
		}
	}
	if len(keys) > 1 && slices.Contains(keys, "enter") {
		return Action{}, invalidCommand(keyUsage)
	}
	return Action{Kind: KindKey, Keys: keys}, nil
}

func parseSlash(trimmed string) (Action, error) {
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "/slash"))
	if payload == "" {
		return Action{}, invalidCommand("/slash 用法: /slash TEXT")
	}
	return Action{Kind: KindPrompt, Text: "/" + payload}, nil
}

func normalizeKey(key string) string {
	switch key {
	case "dn":
		return "down"
	case "sp":
		return "space"
	default:
		return key
	}
}

func fieldsAt(fields []string, index int) string {
	if len(fields) <= index {
		return ""
	}
	return fields[index]
}

func isASCIIUnsignedInteger(value string) bool {
	if value == "" {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func isAllowedKey(value string) bool {
	switch value {
	case "up", "down", "enter", "esc", "space":
		return true
	}
	if len(value) != 1 {
		return false
	}
	return value[0] >= 'A' && value[0] <= 'Z' ||
		value[0] >= 'a' && value[0] <= 'z' ||
		value[0] >= '0' && value[0] <= '9'
}

func invalidCommand(message string) error {
	return fmt.Errorf("%w：%s", ErrInvalidCommand, message)
}
