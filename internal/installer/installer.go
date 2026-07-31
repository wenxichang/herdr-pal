package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/config"
)

const herdrConfigCheckOutputLimit = 8 * 1024

// Request 描述一体化安装器要生成的本机配置。
type Request struct {
	ClientConfigPath string
	HerdrConfigPath  string
	HerdrBinaryPath  string
	RelayURL         string
	RelayKey         string
}

// Result 返回本次写入保留的旧配置备份路径。
type Result struct {
	ClientBackupPath string
	HerdrBackupPath  string
}

// Options 提供安装配置过程中的可测试时钟和 Herdr 校验入口。
type Options struct {
	Now              func() time.Time
	CheckHerdrConfig func(context.Context, string, string, []byte) error
}

// Apply 在全部候选配置通过校验后，以可恢复顺序更新 Herdr 和 Herdr Pal 配置。
func Apply(ctx context.Context, request Request, options Options) (Result, error) {
	if ctx == nil {
		return Result{}, fmt.Errorf("安装配置 context 不能为空")
	}
	request.ClientConfigPath = strings.TrimSpace(request.ClientConfigPath)
	request.HerdrConfigPath = strings.TrimSpace(request.HerdrConfigPath)
	request.HerdrBinaryPath = strings.TrimSpace(request.HerdrBinaryPath)
	if request.ClientConfigPath == "" || request.HerdrConfigPath == "" || request.HerdrBinaryPath == "" {
		return Result{}, fmt.Errorf("安装配置路径不能为空")
	}
	if filepath.Clean(request.ClientConfigPath) == filepath.Clean(request.HerdrConfigPath) {
		return Result{}, fmt.Errorf("Herdr 与 Herdr Pal 配置路径不能相同")
	}

	clientExisting, err := readOptionalRegularFile(request.ClientConfigPath)
	if err != nil {
		return Result{}, err
	}
	herdrExisting, err := readOptionalRegularFile(request.HerdrConfigPath)
	if err != nil {
		return Result{}, err
	}
	clientCandidate, err := mergeClientConfig(clientExisting, request.RelayURL, request.RelayKey)
	if err != nil {
		return Result{}, err
	}
	if err := validateClientConfig(clientCandidate); err != nil {
		return Result{}, err
	}
	herdrCandidate, err := mergeHerdrConfig(herdrExisting)
	if err != nil {
		return Result{}, err
	}
	checkHerdrConfig := options.CheckHerdrConfig
	if checkHerdrConfig == nil {
		checkHerdrConfig = runHerdrConfigCheck
	}
	if err := checkHerdrConfig(ctx, request.HerdrBinaryPath, request.HerdrConfigPath, herdrCandidate); err != nil {
		return Result{}, fmt.Errorf("校验 Herdr 配置 %s: %w", request.HerdrConfigPath, err)
	}

	now := time.Now()
	if options.Now != nil {
		now = options.Now()
	}
	result := Result{}
	result.HerdrBackupPath, err = writePrivateFile(request.HerdrConfigPath, herdrCandidate, now)
	if err != nil {
		return Result{}, err
	}
	result.ClientBackupPath, err = writePrivateFile(request.ClientConfigPath, clientCandidate, now)
	if err == nil {
		return result, nil
	}
	restoreErr := restoreBackup(request.HerdrConfigPath, result.HerdrBackupPath)
	if restoreErr != nil {
		return Result{}, errors.Join(err, restoreErr)
	}
	return Result{}, err
}

func readOptionalRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("检查配置文件 %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("配置文件 %s 不是普通文件", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}
	return data, nil
}

func validateClientConfig(candidate []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(candidate))
	decoder.DisallowUnknownFields()
	var loaded config.ClientConfig
	if err := decoder.Decode(&loaded); err != nil {
		return fmt.Errorf("校验 Herdr Pal 配置: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("校验 Herdr Pal 配置: 包含尾随内容")
	}
	return nil
}

func runHerdrConfigCheck(ctx context.Context, herdrBinaryPath, targetPath string, candidate []byte) error {
	directory, err := os.MkdirTemp("", "herdr-pal-config-check-*")
	if err != nil {
		return fmt.Errorf("创建 Herdr 配置校验目录: %w", err)
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("设置 Herdr 配置校验目录权限: %w", err)
	}
	candidatePath := filepath.Join(directory, filepath.Base(targetPath))
	if err := os.WriteFile(candidatePath, candidate, 0o600); err != nil {
		return fmt.Errorf("写入 Herdr 候选配置: %w", err)
	}
	command := exec.CommandContext(ctx, herdrBinaryPath, "config", "check")
	command.Env = replaceEnvironment(os.Environ(), "HERDR_CONFIG_PATH", candidatePath)
	var output limitedBuffer
	output.limit = herdrConfigCheckOutputLimit
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := strings.TrimSpace(output.String())
		if reason == "" {
			return fmt.Errorf("执行 herdr config check: %w", err)
		}
		return fmt.Errorf("执行 herdr config check: %w: %s", err, reason)
	}
	return nil
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
			buffer.truncated = true
		}
		_, _ = buffer.buffer.Write(data)
	} else if originalLength > 0 {
		buffer.truncated = true
	}
	return originalLength, nil
}

func (buffer *limitedBuffer) String() string {
	if buffer.truncated {
		return buffer.buffer.String() + "\n[输出已截断]"
	}
	return buffer.buffer.String()
}
