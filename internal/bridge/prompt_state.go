package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/session"
)

const (
	promptStateRetryCount    = 3
	promptStateRetryInterval = 500 * time.Millisecond
	promptRecoveryDelay      = 3 * time.Second
)

// 仅表示补键前观察到身份变化；提交已确认时由 Service 复用安全的会话重绑定校验。
var errPromptTargetChanged = errors.New("prompt 确认期间 Agent 身份变化")

func (s *Service) confirmPromptState(ctx context.Context, client HerdrAPI, target session.Target, before herdr.AgentInfo, userID string) (herdr.AgentInfo, error) {
	after, err := retryPromptStateChange(ctx, client, target, before)
	if errors.Is(err, session.ErrSelectionInvalid) {
		return after, errPromptTargetChanged
	}
	if !errors.Is(err, herdr.ErrAgentStateChangeTimeout) {
		return after, err
	}
	if err := waitKeyInterval(ctx, promptRecoveryDelay); err != nil {
		return herdr.AgentInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return herdr.AgentInfo{}, err
	}
	if err := s.guard.ValidateKey("enter"); err != nil {
		return herdr.AgentInfo{}, err
	}
	audits, err := prepareKeyAudits(userID, target, "enter", time.Now().UTC())
	if err != nil {
		return herdr.AgentInfo{}, err
	}

	// 等待期间状态或会话可能变化；必须复核原目标，避免向正在处理消息或等待审批的 Agent 补键。
	after, err = client.GetAgent(ctx, target.PaneID)
	if err != nil {
		s.keyAudit.RecordKeyAudit(audits.failed)
		return herdr.AgentInfo{}, err
	}
	if !session.MatchesAgent(target, after) {
		s.keyAudit.RecordKeyAudit(audits.rejected)
		return after, errPromptTargetChanged
	}
	if promptStateChanged(before, after) {
		s.keyAudit.RecordKeyAudit(audits.rejected)
		return after, nil
	}
	if err := ctx.Err(); err != nil {
		s.keyAudit.RecordKeyAudit(audits.failed)
		return herdr.AgentInfo{}, err
	}
	if err := client.SendKey(ctx, target.PaneID, "enter"); err != nil {
		s.keyAudit.RecordKeyAudit(audits.failed)
		return herdr.AgentInfo{}, err
	}
	s.keyAudit.RecordKeyAudit(audits.sent)
	return retryPromptStateChange(ctx, client, target, before)
}

// 每轮只复查状态，补发 Enter 的决策由两轮之间的目标与状态校验负责。
func retryPromptStateChange(ctx context.Context, client HerdrAPI, target session.Target, before herdr.AgentInfo) (herdr.AgentInfo, error) {
	timer := time.NewTimer(promptStateRetryInterval)
	defer timer.Stop()
	for attempt := 0; attempt < promptStateRetryCount; attempt++ {
		select {
		case <-ctx.Done():
			return herdr.AgentInfo{}, ctx.Err()
		case <-timer.C:
		}
		if err := ctx.Err(); err != nil {
			return herdr.AgentInfo{}, err
		}
		after, err := client.GetAgent(ctx, target.PaneID)
		if err != nil {
			return herdr.AgentInfo{}, err
		}
		if !session.MatchesAgent(target, after) {
			return after, session.ErrSelectionInvalid
		}
		if promptStateChanged(before, after) {
			return after, nil
		}
		timer.Reset(promptStateRetryInterval)
	}
	return herdr.AgentInfo{}, herdr.ErrAgentStateChangeTimeout
}
