// Package dsh 将本机 dsh 插件的受限接口适配为 Pal 会话能力。
package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Session 是插件提供的会话元数据；Incarnation 区分重建后的同名会话。
type Session struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	Title       string `json:"title"`
	Workspace   string `json:"workspace"`
	Status      string `json:"status"`
	Revision    string `json:"revision"`
}

// Client 只访问固定的本机端点；输入请求不自动重试。
type Client struct {
	endpoint, token string
	http            *http.Client
}

// ValidateConfig 限制为 IPv4 回环 HTTP 地址及高熵认证 token。
func ValidateConfig(endpoint, token string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("dsh.url 必须是 http://127.0.0.1:端口，不能包含路径或凭据")
	}
	if len(token) < 32 || len(token) > 256 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return errors.New("dsh.token 必须为 32 到 256 个可打印 ASCII 字符")
	}
	return nil
}

// NewClient 创建不使用代理、不跟随重定向的本机客户端。
func NewClient(endpoint, token string) (*Client, error) {
	if err := ValidateConfig(endpoint, token); err != nil {
		return nil, err
	}
	return &Client{endpoint: endpoint, token: token, http: &http.Client{Timeout: 5 * time.Second,
		Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("dsh 本地连接失败；未自动重试")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("dsh 拒绝请求（HTTP %d）；请检查会话状态或重新 /ls", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return errors.New("dsh 响应读取失败")
	}
	if len(data) > 1<<20 {
		return errors.New("dsh 响应过大")
	}
	if err := json.Unmarshal(data, output); err != nil {
		return errors.New("dsh 响应格式无效")
	}
	return nil
}

// Sessions 读取完整目录，拒绝不兼容协议和重复会话。
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var result struct {
		Protocol int       `json:"protocol"`
		Sessions []Session `json:"sessions"`
	}
	if err := c.call(ctx, "sessions", struct{}{}, &result); err != nil {
		return nil, err
	}
	if result.Protocol != 1 || len(result.Sessions) > 256 {
		return nil, errors.New("dsh 协议版本或会话数量无效")
	}
	seen := map[string]bool{}
	for i, s := range result.Sessions {
		if s.ID == "" || s.Incarnation == "" || len(s.Incarnation) > 128 || strings.IndexFunc(s.Incarnation, unicode.IsControl) >= 0 || seen[s.ID] || (s.Status != "idle" && s.Status != "working" && s.Status != "blocked") {
			return nil, errors.New("dsh 会话目录无效")
		}
		seen[s.ID] = true
		result.Sessions[i].Title = displayLabel(s.Title)
		result.Sessions[i].Workspace = displayLabel(s.Workspace)
	}
	return result.Sessions, nil
}

func displayLabel(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	for len(value) > 512 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

// Prompt 将普通文本提交一次，成功仅表示 dsh 已接收。
func (c *Client) Prompt(ctx context.Context, s Session, key, text string) error {
	var result struct {
		Accepted bool `json:"accepted"`
	}
	err := c.call(ctx, "prompt", map[string]string{"id": s.ID, "incarnation": s.Incarnation, "request_id": key, "text": text}, &result)
	if err != nil {
		return err
	}
	if !result.Accepted {
		return errors.New("dsh 未确认接收消息")
	}
	return nil
}

// Read 读取受限的对话文本，不暴露工具原始结果或隐藏推理。
func (c *Client) Read(ctx context.Context, s Session) (string, error) {
	var result struct {
		Text string `json:"text"`
	}
	err := c.call(ctx, "read", map[string]string{"id": s.ID, "incarnation": s.Incarnation}, &result)
	return result.Text, err
}
