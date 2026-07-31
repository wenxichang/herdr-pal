package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/config"
	"github.com/wenxichang/herdr-pal/internal/installer"
)

const setupInputLimit = 4096

type setupExecutor func(context.Context, installer.Request, installer.Options) (installer.Result, error)

func runSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, execute setupExecutor) int {
	flags := flag.NewFlagSet("herdr-pal setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	relayURL := flags.String("url", "", "HPRP Server 的 wss:// 地址")
	clientConfigPath := flags.String("config", "", "Herdr Pal JSON 配置路径")
	herdrConfigPath := flags.String("herdr-config", "", "Herdr config.toml 路径")
	herdrBinaryPath := flags.String("herdr-bin", "", "用于校验配置的 Herdr 二进制路径")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "用法: herdr-pal setup --url wss://server/hprp --herdr-bin /path/to/herdr [--config path] [--herdr-config path]")
		fmt.Fprintln(stderr, "机器 Key 必须通过标准输入的第一行传入。")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.Usage()
			return 0
		}
		fmt.Fprintln(stderr, "安装配置参数错误。")
		flags.Usage()
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*relayURL) == "" || strings.TrimSpace(*herdrBinaryPath) == "" {
		fmt.Fprintln(stderr, "安装配置缺少必填参数。")
		flags.Usage()
		return 2
	}
	if execute == nil {
		fmt.Fprintln(stderr, "安装配置执行器无效。")
		return 1
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	key, err := readSetupKey(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "读取机器 Key 失败：%s\n", err)
		return 2
	}
	if *clientConfigPath == "" {
		*clientConfigPath, err = config.DefaultClientPath()
		if err != nil {
			fmt.Fprintf(stderr, "确定 Herdr Pal 配置路径失败：%s\n", err)
			return 2
		}
	}
	if *herdrConfigPath == "" {
		*herdrConfigPath, err = installer.DefaultHerdrConfigPath(nil)
		if err != nil {
			fmt.Fprintf(stderr, "确定 Herdr 配置路径失败：%s\n", err)
			return 2
		}
	}
	result, err := execute(ctx, installer.Request{
		ClientConfigPath: *clientConfigPath,
		HerdrConfigPath:  *herdrConfigPath,
		HerdrBinaryPath:  *herdrBinaryPath,
		RelayURL:         strings.TrimSpace(*relayURL),
		RelayKey:         key,
	}, installer.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "安装配置失败：%s\n", redactSetupSecret(err.Error(), key))
		return 1
	}
	fmt.Fprintln(stdout, "安装配置已更新：")
	fmt.Fprintf(stdout, "Herdr Pal: %s\n", *clientConfigPath)
	fmt.Fprintf(stdout, "Herdr: %s\n", *herdrConfigPath)
	if result.ClientBackupPath != "" {
		fmt.Fprintf(stdout, "Herdr Pal 备份: %s\n", result.ClientBackupPath)
	}
	if result.HerdrBackupPath != "" {
		fmt.Fprintf(stdout, "Herdr 备份: %s\n", result.HerdrBackupPath)
	}
	return 0
}

func readSetupKey(input io.Reader) (string, error) {
	reader := bufio.NewReader(io.LimitReader(input, setupInputLimit+1))
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(line) > setupInputLimit {
		return "", fmt.Errorf("机器 Key 过长")
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", fmt.Errorf("机器 Key 不能为空")
	}
	rest, readErr := io.ReadAll(reader)
	if readErr != nil {
		return "", readErr
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", fmt.Errorf("机器 Key 后存在多余输入")
	}
	return key, nil
}

func redactSetupSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[已隐藏]")
}
