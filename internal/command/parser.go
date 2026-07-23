// Package command 负责将企业微信文本解析为受限的桥接动作。
package command

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// KindPrompt 请求发送普通文本提示。
	KindPrompt
)

// Action 是输入文本对应的桥接动作。
type Action struct {
	Kind  Kind
	Index int
	Key   string
	Text  string
}

// ErrInvalidCommand 表示输入为空或命令格式不受支持。
var ErrInvalidCommand = errors.New("无效命令")

// Parse 将输入文本解析为受限动作。非命令文本会保留原始内容作为提示。
func Parse(input string) (Action, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return Action{}, invalidCommand("命令不能为空")
	}
	if !strings.HasPrefix(trimmed, "/") {
		return Action{Kind: KindPrompt, Text: input}, nil
	}

	fields := strings.Fields(trimmed)
	command := fields[0]
	switch command {
	case "/ls":
		return parseAlias(fields, command, Action{Kind: KindList})
	case "/con":
		return parseAlias(fields, command, Action{Kind: KindContent})
	case "/pageup":
		return parseAlias(fields, command, Action{Kind: KindPageUp})
	case "/pagedn":
		return parseAlias(fields, command, Action{Kind: KindPageDown})
	case "/keyup":
		return parseAlias(fields, command, Action{Kind: KindKey, Key: "up"})
	case "/keydn":
		return parseAlias(fields, command, Action{Kind: KindKey, Key: "down"})
	case "/enter":
		return parseAlias(fields, command, Action{Kind: KindKey, Key: "enter"})
	case "/esc":
		return parseAlias(fields, command, Action{Kind: KindKey, Key: "esc"})
	case "/space":
		return parseAlias(fields, command, Action{Kind: KindKey, Key: "space"})
	case "/sel":
		return parseSelect(fields)
	case "/key":
		return parseKey(fields)
	default:
		return Action{}, invalidCommand("未知命令")
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

	index, err := strconv.Atoi(fields[1])
	if err != nil || index <= 0 {
		return Action{}, invalidCommand("/sel 用法: /sel N")
	}
	return Action{Kind: KindSelect, Index: index}, nil
}

func parseKey(fields []string) (Action, error) {
	if len(fields) != 2 || !isAllowedKey(fieldsAt(fields, 1)) {
		return Action{}, invalidCommand("/key 用法: /key KEY")
	}
	return Action{Kind: KindKey, Key: fields[1]}, nil
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
