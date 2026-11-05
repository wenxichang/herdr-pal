package installer

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	managedSidecarBegin = "# BEGIN HERDR PAL MANAGED SIDECAR"
	managedSidecarEnd   = "# END HERDR PAL MANAGED SIDECAR"
	managedSidecarBlock = managedSidecarBegin + "\n" +
		"[[sidecar]]\n" +
		"command = [\"herdr-pal\"]\n" +
		managedSidecarEnd + "\n"
)

type herdrDocument struct {
	Sidecars []struct {
		Command []string `toml:"command"`
	} `toml:"sidecar"`
}

func mergeHerdrConfig(existing []byte) ([]byte, error) {
	text := string(existing)
	beginCount := strings.Count(text, managedSidecarBegin)
	endCount := strings.Count(text, managedSidecarEnd)
	if beginCount != endCount || beginCount > 1 {
		return nil, fmt.Errorf("Herdr 配置中的受管 Sidecar 标记无效")
	}

	var merged []byte
	if beginCount == 1 {
		beginIndex := strings.Index(text, managedSidecarBegin)
		endIndex := strings.Index(text, managedSidecarEnd)
		if beginIndex < 0 || endIndex < beginIndex || !markerStartsLine(text, beginIndex) || !markerEndsLine(text, endIndex, len(managedSidecarEnd)) {
			return nil, fmt.Errorf("Herdr 配置中的受管 Sidecar 标记顺序无效")
		}
		endCut := endIndex + len(managedSidecarEnd)
		if strings.HasPrefix(text[endCut:], "\r\n") {
			endCut += 2
		} else if strings.HasPrefix(text[endCut:], "\n") {
			endCut++
		}
		merged = []byte(text[:beginIndex] + managedSidecarBlock + text[endCut:])
	} else {
		document, err := decodeHerdrDocument(existing)
		if err != nil {
			return nil, err
		}
		for _, sidecar := range document.Sidecars {
			if len(sidecar.Command) == 1 && sidecar.Command[0] == "herdr-pal" {
				return append([]byte(nil), existing...), nil
			}
		}
		var output bytes.Buffer
		output.Write(existing)
		if len(existing) > 0 && existing[len(existing)-1] != '\n' {
			output.WriteByte('\n')
		}
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		output.WriteString(managedSidecarBlock)
		merged = output.Bytes()
	}
	if _, err := decodeHerdrDocument(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func decodeHerdrDocument(content []byte) (herdrDocument, error) {
	var document herdrDocument
	if _, err := toml.Decode(string(content), &document); err != nil {
		return herdrDocument{}, fmt.Errorf("解析 Herdr 配置: %w", err)
	}
	return document, nil
}

func removeManagedPalSidecar(existing []byte) ([]byte, error) {
	if _, err := decodeHerdrDocument(existing); err != nil {
		return nil, err
	}
	text := string(existing)
	beginCount, endCount := strings.Count(text, managedSidecarBegin), strings.Count(text, managedSidecarEnd)
	if beginCount == 0 && endCount == 0 {
		return append([]byte(nil), existing...), nil
	}
	if beginCount != 1 || endCount != 1 {
		return nil, fmt.Errorf("Herdr 配置中的受管 Sidecar 标记无效")
	}
	begin, end := strings.Index(text, managedSidecarBegin), strings.Index(text, managedSidecarEnd)
	if end <= begin || !markerStartsLine(text, begin) || !markerStartsLine(text, end) ||
		!markerEndsLine(text, begin, len(managedSidecarBegin)) || !markerEndsLine(text, end, len(managedSidecarEnd)) {
		return nil, fmt.Errorf("Herdr 配置中的受管 Sidecar 标记顺序无效")
	}
	// 前缀必须独立可解析，避免把 TOML 多行字符串中的标记误认为受管块。
	prefix, err := decodeHerdrDocument([]byte(text[:begin]))
	if err != nil {
		return nil, err
	}
	var block herdrDocument
	metadata, err := toml.Decode(text[begin:end], &block)
	if err != nil || len(metadata.Undecoded()) != 0 || len(block.Sidecars) != 1 ||
		len(block.Sidecars[0].Command) != 1 || block.Sidecars[0].Command[0] != "herdr-pal" {
		return nil, fmt.Errorf("受管 Sidecar 块包含非标准 Pal 配置，请手工迁移")
	}
	end += len(managedSidecarEnd)
	if strings.HasPrefix(text[end:], "\r\n") {
		end += 2
	} else if strings.HasPrefix(text[end:], "\n") {
		end++
	}
	merged := []byte(text[:begin] + text[end:])
	// TOML 表的作用域跨越注释标记；删除块不能把后续字段归入另一张表。
	var original, candidate map[string]any
	if _, err := toml.Decode(text, &original); err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(merged), &candidate); err != nil {
		return nil, err
	}
	sidecars, ok := original["sidecar"].([]map[string]any)
	index := len(prefix.Sidecars)
	if !ok || index >= len(sidecars) || len(sidecars[index]) != 1 {
		return nil, fmt.Errorf("受管 Sidecar 在标记外还有配置，请手工迁移")
	}
	sidecars = append(sidecars[:index], sidecars[index+1:]...)
	if len(sidecars) == 0 {
		delete(original, "sidecar")
	} else {
		original["sidecar"] = sidecars
	}
	if !reflect.DeepEqual(original, candidate) {
		return nil, fmt.Errorf("移除 Sidecar 会改变其他配置的作用域，请手工迁移")
	}
	return merged, nil
}

func markerStartsLine(text string, index int) bool {
	return index == 0 || text[index-1] == '\n'
}

func markerEndsLine(text string, index, markerLength int) bool {
	end := index + markerLength
	return end == len(text) || text[end] == '\n' || text[end] == '\r'
}
