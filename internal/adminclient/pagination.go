package adminclient

import (
	"context"
	"errors"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

const maxAutomaticPages = 10000

// ListKeys 在同一连接中自动遍历全部 Key 分页。
func (client *Client) ListKeys(ctx context.Context, params adminproto.KeyListParams) (adminproto.KeyListResult, error) {
	session, err := client.Open(ctx)
	if err != nil {
		return adminproto.KeyListResult{}, err
	}
	defer session.Close()
	var aggregate adminproto.KeyListResult
	seen := make(map[string]struct{})
	for page := 0; page < maxAutomaticPages; page++ {
		var current adminproto.KeyListResult
		if err := session.Call(ctx, adminproto.MethodKeyList, params, &current); err != nil {
			return adminproto.KeyListResult{}, err
		}
		aggregate.ObservedAt = current.ObservedAt
		aggregate.Items = append(aggregate.Items, current.Items...)
		if current.NextPageToken == "" {
			return aggregate, nil
		}
		if _, exists := seen[current.NextPageToken]; exists {
			return adminproto.KeyListResult{}, protocolError(errors.New("Key 分页 token 重复"))
		}
		seen[current.NextPageToken] = struct{}{}
		params.PageToken = current.NextPageToken
	}
	return adminproto.KeyListResult{}, protocolError(errors.New("Key 自动分页超过页数限制"))
}

// ListConnections 在同一连接中自动遍历全部 HPRP 连接分页。
func (client *Client) ListConnections(ctx context.Context, params adminproto.ConnectionListParams) (adminproto.ConnectionListResult, error) {
	session, err := client.Open(ctx)
	if err != nil {
		return adminproto.ConnectionListResult{}, err
	}
	defer session.Close()
	var aggregate adminproto.ConnectionListResult
	seen := make(map[string]struct{})
	for page := 0; page < maxAutomaticPages; page++ {
		var current adminproto.ConnectionListResult
		if err := session.Call(ctx, adminproto.MethodConnectionList, params, &current); err != nil {
			return adminproto.ConnectionListResult{}, err
		}
		aggregate.ObservedAt = current.ObservedAt
		aggregate.Items = append(aggregate.Items, current.Items...)
		if current.NextPageToken == "" {
			return aggregate, nil
		}
		if _, exists := seen[current.NextPageToken]; exists {
			return adminproto.ConnectionListResult{}, protocolError(errors.New("Connection 分页 token 重复"))
		}
		seen[current.NextPageToken] = struct{}{}
		params.PageToken = current.NextPageToken
	}
	return adminproto.ConnectionListResult{}, protocolError(errors.New("Connection 自动分页超过页数限制"))
}

// ListSessions 在同一连接中自动遍历全部 Agent 会话分页。
func (client *Client) ListSessions(ctx context.Context, params adminproto.SessionListParams) (adminproto.SessionListResult, error) {
	session, err := client.Open(ctx)
	if err != nil {
		return adminproto.SessionListResult{}, err
	}
	defer session.Close()
	var aggregate adminproto.SessionListResult
	seen := make(map[string]struct{})
	for page := 0; page < maxAutomaticPages; page++ {
		var current adminproto.SessionListResult
		if err := session.Call(ctx, adminproto.MethodSessionList, params, &current); err != nil {
			return adminproto.SessionListResult{}, err
		}
		aggregate.ObservedAt = current.ObservedAt
		aggregate.Items = append(aggregate.Items, current.Items...)
		if current.NextPageToken == "" {
			return aggregate, nil
		}
		if _, exists := seen[current.NextPageToken]; exists {
			return adminproto.SessionListResult{}, protocolError(errors.New("Session 分页 token 重复"))
		}
		seen[current.NextPageToken] = struct{}{}
		params.PageToken = current.NextPageToken
	}
	return adminproto.SessionListResult{}, protocolError(errors.New("Session 自动分页超过页数限制"))
}
