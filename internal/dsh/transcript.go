package dsh

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	// transcriptMessageLimit 限制最终展示的聊天消息条数。
	transcriptMessageLimit = 100
	// transcriptUTF16Limit 限制最终正文长度，与 IM 消息展示预算一致。
	transcriptUTF16Limit = 16000
)

// errTranscriptMalformed 表示原生事件记录无法安全投影；调用方不得返回部分正文。
var errTranscriptMalformed = errors.New("dsh 会话记录格式无效")

// transcriptMessage 是一条可展示的聊天消息；seq 用于应用 surfaceOp 替换范围。
type transcriptMessage struct {
	seq  uint64
	role string
	text string
}

// transcriptRecord 是 session/page records 中单条事件记录的最小投影。
type transcriptRecord struct {
	Event struct {
		Seq       *uint64         `json:"seq"`
		Type      string          `json:"type"`
		Data      json.RawMessage `json:"data"`
		SurfaceOp json.RawMessage `json:"surfaceOp"`
	} `json:"event"`
}

// userMessageData 只解析真实用户输入判定与文本块所需的字段。
type userMessageData struct {
	Source struct {
		Kind string `json:"kind"`
	} `json:"source"`
	Content json.RawMessage `json:"content"`
}

// assistantMessageData 只解析助手消息正文；推理与工具块在文本块过滤时丢弃。
type assistantMessageData struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock 是消息内容块的最小投影。
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// conversationText 把原生事件记录投影成「用户：/助手：」聊天正文。
//
// 只保留 source.kind=user 的真实用户输入和 assistant/message 的 text 块；
// 工具、推理、系统/开发者注入、压缩摘要、图片和未知事件都不会出现。
// 处理 surfaceOp.replace 后保留最后 100 条消息，并截取末尾 16000 个 UTF-16
// 代码单元；任何记录格式错误都整体失败，不返回部分正文。
func conversationText(records []json.RawMessage) (string, error) {
	messages, err := projectMessages(records)
	if err != nil {
		return "", err
	}
	if len(messages) > transcriptMessageLimit {
		messages = messages[len(messages)-transcriptMessageLimit:]
	}

	var builder strings.Builder
	for index, message := range messages {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(message.role)
		builder.WriteString("：")
		builder.WriteString(message.text)
	}
	return truncateUTF16Tail(builder.String(), transcriptUTF16Limit), nil
}

// projectMessages 按事件顺序投影消息，并让每条 surfaceOp.replace 删除范围内旧消息。
//
// 跨页读取时应先把各页按时间顺序拼接再调用，替换范围才能作用于更早的页。
func projectMessages(records []json.RawMessage) ([]transcriptMessage, error) {
	messages := make([]transcriptMessage, 0, min(len(records), transcriptMessageLimit))
	for index, raw := range records {
		var record transcriptRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条记录不是有效 JSON", errTranscriptMalformed, index)
		}
		if record.Event.Seq == nil || record.Event.Type == "" {
			return nil, fmt.Errorf("%w: 第 %d 条记录缺少事件序号或类型", errTranscriptMalformed, index)
		}
		seq := *record.Event.Seq
		if err := applySurfaceReplacement(&messages, seq, record.Event.SurfaceOp); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条记录 %v", errTranscriptMalformed, index, err)
		}

		switch record.Event.Type {
		case "user/message":
			var data userMessageData
			if err := json.Unmarshal(record.Event.Data, &data); err != nil {
				return nil, fmt.Errorf("%w: 第 %d 条用户消息格式无效", errTranscriptMalformed, index)
			}
			// source.kind 必须明确为 user；注入、定时上下文与未知来源一律丢弃。
			if data.Source.Kind != "user" {
				continue
			}
			blocks, err := decodeContentBlocks(data.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: 第 %d 条用户消息 %v", errTranscriptMalformed, index, err)
			}
			appendTextBlocks(&messages, seq, "用户", blocks)
		case "assistant/message":
			var data assistantMessageData
			if err := json.Unmarshal(record.Event.Data, &data); err != nil {
				return nil, fmt.Errorf("%w: 第 %d 条助手消息格式无效", errTranscriptMalformed, index)
			}
			blocks, err := decodeContentBlocks(data.Message.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: 第 %d 条助手消息 %v", errTranscriptMalformed, index, err)
			}
			appendTextBlocks(&messages, seq, "助手", blocks)
		}
	}
	return messages, nil
}

// applySurfaceReplacement 在投影当前事件前删除被替换的旧消息。
//
// 只接受 append 与 replace 两种原生操作；替换范围必须落在当前事件之前，
// 未知或越界操作视为协议不兼容并失败关闭。
func applySurfaceReplacement(messages *[]transcriptMessage, seq uint64, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var op string
		if err := json.Unmarshal(trimmed, &op); err != nil || op != "append" {
			return errors.New("surfaceOp 不是 append")
		}
		return nil
	}

	var op struct {
		Op       string  `json:"op"`
		StartSeq *uint64 `json:"startSeq"`
		EndSeq   *uint64 `json:"endSeq"`
	}
	if err := json.Unmarshal(trimmed, &op); err != nil {
		return errors.New("surfaceOp 格式无效")
	}
	if op.Op != "replace" {
		return errors.New("surfaceOp 操作未知")
	}
	if op.StartSeq == nil || op.EndSeq == nil || *op.StartSeq > *op.EndSeq || *op.EndSeq >= seq {
		return errors.New("surfaceOp 替换范围无效")
	}

	start, end := *op.StartSeq, *op.EndSeq
	kept := (*messages)[:0]
	for _, message := range *messages {
		if message.seq < start || message.seq > end {
			kept = append(kept, message)
		}
	}
	*messages = kept
	return nil
}

// decodeContentBlocks 解析消息内容块；空内容不产生消息。
func decodeContentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, errors.New("内容不是文本块数组")
	}
	return blocks, nil
}

// appendTextBlocks 只追加非空文本块，并清理 ANSI 转义与控制字符。
func appendTextBlocks(out *[]transcriptMessage, seq uint64, role string, blocks []contentBlock) {
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		text := stripTranscriptControl(block.Text)
		if text == "" {
			continue
		}
		*out = append(*out, transcriptMessage{seq: seq, role: role, text: text})
	}
}

// stripTranscriptControl 删除 ANSI 转义序列与 C0/C1 控制字符，保留换行和制表符。
func stripTranscriptControl(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for index := 0; index < len(text); {
		if text[index] == 0x1b {
			index = skipEscapeSequence(text, index)
			continue
		}
		current, size := utf8.DecodeRuneInString(text[index:])
		if current == utf8.RuneError && size == 1 {
			index++
			continue
		}
		if current == '\n' || current == '\t' || !unicode.IsControl(current) {
			builder.WriteString(text[index : index+size])
		}
		index += size
	}
	return builder.String()
}

// skipEscapeSequence 返回 index 处转义序列之后的字节位置。
//
// CSI 在终止字节（0x40–0x7E）结束；OSC/DCS/PM/APC 在 BEL 或 ESC \ 结束。
// 遇到换行即停止，避免畸形序列吞掉后续正文。
func skipEscapeSequence(text string, index int) int {
	next := index + 1
	if next >= len(text) {
		return next
	}
	switch text[next] {
	case '[':
		for cursor := next + 1; cursor < len(text); cursor++ {
			current := text[cursor]
			if current == '\n' || current == '\r' {
				return cursor
			}
			if current >= 0x40 && current <= 0x7e {
				return cursor + 1
			}
		}
		return len(text)
	case ']', 'P', '^', '_':
		for cursor := next + 1; cursor < len(text); cursor++ {
			switch text[cursor] {
			case '\a':
				return cursor + 1
			case '\n':
				return cursor
			case 0x1b:
				if cursor+1 < len(text) && text[cursor+1] == '\\' {
					return cursor + 2
				}
			}
		}
		return len(text)
	default:
		// ESC ( / ESC ) / ESC # 后还有一个设计字符，其余按两字节转义处理。
		if text[next] == '(' || text[next] == ')' || text[next] == '#' {
			if next+1 < len(text) {
				return next + 2
			}
		}
		return next + 1
	}
}

// truncateUTF16Tail 返回末尾不超过 maxUnits 个 UTF-16 代码单元的后缀。
//
// 计数按 UTF-16 规则进行；边界落在代理对中间时丢弃整个字符，保证结果
// 始终是合法 UTF-8 且不会以半个 emoji 开头。
func truncateUTF16Tail(text string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	units := 0
	start := len(text)
	for start > 0 {
		current, size := utf8.DecodeLastRuneInString(text[:start])
		width := utf16.RuneLen(current)
		if width < 1 {
			width = 1
		}
		if units+width > maxUnits {
			break
		}
		units += width
		start -= size
	}
	return text[start:]
}
