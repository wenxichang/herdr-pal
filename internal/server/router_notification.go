package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/notificationprefs"
)

// NotificationPreferences 提供业务组合的显式状态、事务式更新和在线范围核对。
type NotificationPreferences interface {
	Enabled(notificationprefs.Identity) bool
	Lookup(notificationprefs.Identity) (bool, bool)
	SetBatch([]notificationprefs.Identity, bool) error
	Reconcile(string, []string, []notificationprefs.Identity) (notificationprefs.ReconcileResult, error)
}

type notificationAction struct {
	hasSetting         bool
	enabled            bool
	all                bool
	hasExplicitIndexes bool
	indexes            []int
	issues             []NotificationIndexIssue
}

const notificationUsage = "/notify 用法: /notify；/notify on|off；/notify on|off 1,2 3；/notify on|off all"

func parseNotificationAction(fields []string) (notificationAction, error) {
	action := notificationAction{}
	if len(fields) == 1 {
		return action, nil
	}
	if len(fields) < 2 || (fields[1] != "on" && fields[1] != "off") {
		return action, errors.New(notificationUsage)
	}
	action.hasSetting = true
	action.enabled = fields[1] == "on"
	if len(fields) == 2 {
		return action, nil
	}
	raw := strings.Join(fields[2:], " ")
	if strings.ContainsAny(raw, ";；") {
		return action, errors.New(notificationUsage)
	}
	tokens := strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || r == ',' || r == '，' })
	if len(tokens) == 0 {
		return action, errors.New(notificationUsage)
	}
	for _, token := range tokens {
		if token == "all" {
			if len(tokens) != 1 {
				return action, errors.New(notificationUsage)
			}
			action.all = true
			return action, nil
		}
	}
	action.hasExplicitIndexes = true
	seen := make(map[int]bool)
	for _, token := range tokens {
		index, err := positiveASCIIInt(token)
		if err != nil {
			action.issues = append(action.issues, NotificationIndexIssue{Input: token, Reason: "不是有效的正整数编号"})
			continue
		}
		if !seen[index] {
			seen[index] = true
			action.indexes = append(action.indexes, index)
		}
	}
	return action, nil
}

func (router *ConversationRouter) handleNotify(ctx context.Context, message im.IncomingText, action notificationAction) {
	var result NotificationBatchResolution
	if action.all {
		var err error
		result, err = notificationGroups(message.UserID, router.catalog.NotificationInventory(message.UserID).Entries)
		if err != nil {
			router.reply(ctx, message, "通知目标解析失败，请重试。")
			return
		}
	} else if action.hasExplicitIndexes {
		if len(action.indexes) > 0 {
			var err error
			result, err = router.catalog.ResolveNotificationGroups(message.UserID, action.indexes)
			if err != nil {
				router.reply(ctx, message, safeRouterError(err))
				return
			}
		}
	} else {
		entry, err := router.catalog.Selected(message.UserID)
		if err != nil {
			router.reply(ctx, message, safeRouterError(err))
			return
		}
		key, err := notificationIdentity(message.UserID, entry)
		if err != nil {
			router.reply(ctx, message, "通知目标解析失败，请重试。")
			return
		}
		if !action.hasSetting {
			router.reply(ctx, message, fmt.Sprintf("%s\n通知：%s（相同组合共享开关）。", catalogTargetLabel(entry), notificationStateLabel(router.notificationPreferences.Enabled(key))))
			return
		}
		// 一个显式目标仍代表整个业务组合，统计同组合当前的所有会话。
		result.Identities = []notificationprefs.Identity{key}
		for _, current := range router.catalog.NotificationInventory(message.UserID).Entries {
			currentKey, err := notificationIdentity(message.UserID, current)
			if err != nil {
				router.reply(ctx, message, "通知目标解析失败，请重试。")
				return
			}
			if currentKey == key {
				result.AffectedSessions++
			}
		}
		if result.AffectedSessions == 0 {
			router.reply(ctx, message, "目标组合已失效，请重新执行 /ls。")
			return
		}
	}
	result.Skipped = append(action.issues, result.Skipped...)
	if len(result.Identities) == 0 {
		content := "当前没有在线且可设置通知的会话。"
		if len(result.Skipped) > 0 {
			content = "未修改任何通知开关。" + notificationSkippedText(result.Skipped)
		}
		router.reply(ctx, message, content)
		return
	}
	if err := router.notificationPreferences.SetBatch(result.Identities, action.enabled); err != nil {
		router.logger.Warn("通知偏好保存失败", "user_hash", routerHash(message.UserID), "error_type", "notification_preferences_write", "group_count", len(result.Identities))
		router.reply(ctx, message, "通知设置保存失败，有效项均未生效，请重试。"+notificationSkippedText(result.Skipped))
		return
	}
	verb := "开启"
	if !action.enabled {
		verb = "关闭"
	}
	router.logger.Info("通知偏好已更新", "user_hash", routerHash(message.UserID), "group_count", len(result.Identities), "enabled", action.enabled, "skipped_count", len(result.Skipped))
	router.reply(ctx, message, fmt.Sprintf("已%s %d 个组合（当前 %d 个会话），相同组合共享开关。%s", verb, len(result.Identities), result.AffectedSessions, notificationSkippedText(result.Skipped)))
}

func notificationSkippedText(issues []NotificationIndexIssue) string {
	var output strings.Builder
	for _, issue := range issues {
		fmt.Fprintf(&output, "\n跳过 %s：%s。", safeRouterLabel(issue.Input), issue.Reason)
	}
	return output.String()
}

func notificationStateLabel(enabled bool) string {
	if enabled {
		return "✔️ 开启"
	}
	return "✖️ 关闭"
}

func notificationGroups(userID string, entries []CatalogEntry) (NotificationBatchResolution, error) {
	result := NotificationBatchResolution{AffectedSessions: len(entries)}
	seen := make(map[notificationprefs.Identity]bool)
	for _, entry := range entries {
		key, err := notificationIdentity(userID, entry)
		if err != nil {
			return NotificationBatchResolution{}, err
		}
		if !seen[key] {
			seen[key] = true
			result.Identities = append(result.Identities, key)
		}
	}
	return result, nil
}

func isNotificationCommand(content string) bool {
	fields := strings.Fields(content)
	return len(fields) > 0 && fields[0] == "/notify"
}
