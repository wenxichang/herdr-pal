package installer

import (
	"fmt"
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

	merged := append([]byte(nil), existing...)
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
		merged = []byte(text[:beginIndex] + text[endCut:])
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

func markerStartsLine(text string, index int) bool {
	return index == 0 || text[index-1] == '\n'
}

func markerEndsLine(text string, index, markerLength int) bool {
	end := index + markerLength
	return end == len(text) || text[end] == '\n' || text[end] == '\r'
}
