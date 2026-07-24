package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func decodeFile[T any](path string) (T, error) {
	var value T
	file, err := os.Open(path)
	if err != nil {
		return value, fmt.Errorf("读取配置文件: %w", err)
	}
	defer file.Close()
	if err := decodeStrict(file, &value); err != nil {
		return value, err
	}
	return value, nil
}

func decodeStrict(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("解析配置文件: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("配置文件包含多个 JSON 值")
		}
		return fmt.Errorf("配置文件包含尾随内容: %w", err)
	}
	return nil
}
