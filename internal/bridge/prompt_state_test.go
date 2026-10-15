package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

func TestServicePromptStateConfirmationFlow(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		path := "unchanged_response"
		if stalled {
			path = "stalled_response"
		}
		for _, test := range []struct {
			name         string
			changeCheck  int
			status       herdr.AgentStatus
			sequenceOnly bool
		}{
			{name: "first_check", changeCheck: 1, status: herdr.AgentStatusWorking},
			{name: "second_check", changeCheck: 2, status: herdr.AgentStatusWorking},
			{name: "third_check", changeCheck: 3, status: herdr.AgentStatusWorking},
			{name: "changed_during_delay", changeCheck: 4, status: herdr.AgentStatusWorking},
			{name: "blocked_during_delay", changeCheck: 4, status: herdr.AgentStatusBlocked},
			{name: "first_check_after_enter", changeCheck: 5, status: herdr.AgentStatusWorking},
			{name: "second_check_after_enter", changeCheck: 6, status: herdr.AgentStatusWorking},
			{name: "third_check_after_enter", changeCheck: 7, status: herdr.AgentStatusWorking},
			{name: "exhausted"},
			{name: "sequence_only", changeCheck: 2, status: herdr.AgentStatusIdle, sequenceOnly: true},
			{name: "sequence_only_after_enter", changeCheck: 6, status: herdr.AgentStatusIdle, sequenceOnly: true},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					service, fake := newTestService(t)
					selectTarget(t, service)
					current := fake.currentAgent()
					fake.promptResult = current
					if stalled {
						fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled"}
					}
					results := []agentGetResult{{agent: current}}
					for check := 1; check <= 7; check++ {
						after := current
						if check == test.changeCheck {
							after.AgentStatus = test.status
							if test.sequenceOnly {
								after.StateChangeSeq++
							}
						}
						results = append(results, agentGetResult{agent: after})
					}
					fake.setGetResults(results...)
					initialGets := len(fake.gets())
					started := time.Now()

					service.HandleMessage(context.Background(), incoming("confirm-state", "继续执行"))

					checks := test.changeCheck
					if checks == 0 {
						checks = 7
					}
					if elapsed := time.Since(started); elapsed != promptCheckTime(checks) {
						t.Fatalf("确认状态耗时 = %v，期望 %v", elapsed, promptCheckTime(checks))
					}
					if gets := len(fake.gets()) - initialGets; gets != 1+checks {
						t.Fatalf("agent.get 次数 = %d，期望 %d", gets, 1+checks)
					}
					wantKeys := 0
					var wantAudit policy.AuditResult
					if checks == 4 {
						wantAudit = policy.AuditResultRejected
					} else if checks > 4 {
						wantKeys, wantAudit = 1, policy.AuditResultSent
					}
					assertPromptRecoveryEffects(t, service, fake, wantKeys, wantAudit)
					if audits := fakeAuditFromService(t, service).records(); len(audits) == 1 && audits[0].At().Sub(started) != 4500*time.Millisecond {
						t.Fatalf("Enter 恢复时机 = %v，期望发送后 4.5 秒", audits[0].At().Sub(started))
					}
					if wantKeys == 1 && !strings.HasPrefix(strings.Join(fake.callOrder(), ","), "get,prompt,get,get,get,get,key,") {
						t.Fatalf("Enter 前未完成首次检查、三次轮询和等待后复核：%#v", fake.callOrder())
					}
					reply := fakeIMFromService(t, service).lastReply()
					if test.changeCheck == 0 {
						if reply != "消息已送入终端，但未检测到 Agent 状态变化，请检查 Agent 界面。" {
							t.Fatalf("耗尽两轮检查后的回复 = %q", reply)
						}
					} else if !strings.Contains(reply, "已发送，Agent 状态已变为 "+string(test.status)) {
						t.Fatalf("发现状态变化后的回复 = %q", reply)
					}
				})
			})
		}
	}
}

// 第四次读取是等待 3 秒后的补键前复核，后续三次属于第二轮。
func promptCheckTime(check int) time.Duration {
	if check <= 3 {
		return time.Duration(check) * 500 * time.Millisecond
	}
	return 4500*time.Millisecond + time.Duration(check-4)*500*time.Millisecond
}

func TestServicePromptRebindsSessionObservedBeforeRecoveryEnter(t *testing.T) {
	for _, changeCheck := range []int{1, 3, 4} {
		t.Run(fmt.Sprintf("check_%d", changeCheck), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, fake := newTestService(t)
				snapshot := testSnapshotWithSession("session-old")
				service.registry.Replace(snapshot, false)
				fake.setSnapshot(snapshot)
				before := fake.currentAgent()
				before.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-old"}
				fake.setAgent(before)
				fake.promptResult = before
				selectTarget(t, service)
				after := changedAgent(before, herdr.AgentStatusWorking)
				after.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "session-new"}
				results := []agentGetResult{{agent: before}}
				for check := 1; check < changeCheck; check++ {
					results = append(results, agentGetResult{agent: before})
				}
				fake.setGetResults(append(results, agentGetResult{agent: after})...)
				started := time.Now()

				service.HandleMessage(context.Background(), incoming("late-session", "继续执行"))

				selected, err := service.SelectedTarget()
				if err != nil || !session.MatchesAgent(selected, after) {
					t.Fatalf("轮询发现新会话后未自动选择：selected=%#v err=%v", selected, err)
				}
				if reply := fakeIMFromService(t, service).lastReply(); !strings.Contains(reply, "已自动选择新会话") {
					t.Fatalf("新会话回复 = %q", reply)
				}
				if time.Since(started) != promptCheckTime(changeCheck) {
					t.Fatalf("发现新会话后未立即停止：%v", time.Since(started))
				}
				var wantAudit policy.AuditResult
				if changeCheck == 4 {
					wantAudit = policy.AuditResultRejected
				}
				assertPromptRecoveryEffects(t, service, fake, 0, wantAudit)
			})
		})
	}
}

func TestServicePromptRetryStopsOnReplacementOrReadError(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		path := "unchanged_response"
		if stalled {
			path = "stalled_response"
		}
		for _, name := range []string{"occupant", "session", "read_error"} {
			if name == "session" && !stalled {
				continue
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					service, fake := newTestService(t)
					selectTarget(t, service)
					current := fake.currentAgent()
					fake.promptResult = current
					if stalled {
						fake.promptErr = &herdr.APIError{Code: "agent_prompt_stalled"}
					}
					result := agentGetResult{agent: changedAgent(current, herdr.AgentStatusWorking)}
					switch name {
					case "occupant":
						result.agent.TerminalID = "terminal-replaced"
					case "session":
						result.agent.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "new-session"}
					case "read_error":
						result.err = errors.New("private read failure")
					}
					fake.setGetResults(agentGetResult{agent: current}, agentGetResult{agent: current}, result)
					initialGets := len(fake.gets())
					started := time.Now()

					service.HandleMessage(context.Background(), incoming("retry-failed", "继续执行"))

					if elapsed := time.Since(started); elapsed != time.Second || len(fake.gets())-initialGets != 3 {
						t.Fatalf("第二次复查失败后未停止：elapsed=%v gets=%#v", elapsed, fake.gets())
					}
					assertPromptSentOnceWithoutKeys(t, service, fake)
					want := targetChangedMessage
					if name == "read_error" {
						want = unavailableMessage
					} else {
						if !stalled {
							want = targetChangedAfterPromptMessage
						}
						if _, err := service.registry.ValidateSelected(); err == nil {
							t.Fatal("目标更换后未清除选择")
						}
					}
					if reply := fakeIMFromService(t, service).lastReply(); reply != want {
						t.Fatalf("失败回复 = %q，期望 %q", reply, want)
					}
				})
			})
		}
	}
}

func TestServicePromptRetryStopsOnCancellation(t *testing.T) {
	for _, test := range []struct {
		name        string
		cancelAfter time.Duration
		wantGets    int
		wantKeys    int
	}{
		{name: "first_round", cancelAfter: 750 * time.Millisecond, wantGets: 2},
		{name: "recovery_delay", cancelAfter: 3 * time.Second, wantGets: 4},
		{name: "second_round", cancelAfter: 4750 * time.Millisecond, wantGets: 5, wantKeys: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, fake := newTestService(t)
				selectTarget(t, service)
				fake.promptResult = fake.currentAgent()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				timer := time.AfterFunc(test.cancelAfter, cancel)
				defer timer.Stop()
				initialGets := len(fake.gets())
				initialReplies := fakeIMFromService(t, service).replyCount()
				started := time.Now()

				service.HandleMessage(ctx, incoming("retry-cancelled", "继续执行"))

				if elapsed := time.Since(started); elapsed != test.cancelAfter || len(fake.gets())-initialGets != test.wantGets {
					t.Fatalf("取消后未停止：elapsed=%v gets=%#v", elapsed, fake.gets())
				}
				if fakeIMFromService(t, service).replyCount() != initialReplies {
					t.Fatal("取消后仍发送回复")
				}
				var wantAudit policy.AuditResult
				if test.wantKeys == 1 {
					wantAudit = policy.AuditResultSent
				}
				assertPromptRecoveryEffects(t, service, fake, test.wantKeys, wantAudit)
				service.SetHerdr(nil)
			})
		})
	}
}

func TestServicePromptRecoveryStopsOnFailure(t *testing.T) {
	for _, test := range []struct {
		name          string
		failureCheck  int
		readError     bool
		sendError     bool
		sessionChange bool
		wantKeys      int
		wantAudit     policy.AuditResult
	}{
		{name: "replacement_before_enter", failureCheck: 4, wantAudit: policy.AuditResultRejected},
		{name: "read_error_before_enter", failureCheck: 4, readError: true, wantAudit: policy.AuditResultFailed},
		{name: "enter_error", failureCheck: 4, sendError: true, wantKeys: 1, wantAudit: policy.AuditResultFailed},
		{name: "replacement_after_enter", failureCheck: 6, wantKeys: 1, wantAudit: policy.AuditResultSent},
		{name: "session_after_enter", failureCheck: 6, sessionChange: true, wantKeys: 1, wantAudit: policy.AuditResultSent},
		{name: "read_error_after_enter", failureCheck: 6, readError: true, wantKeys: 1, wantAudit: policy.AuditResultSent},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, fake := newTestService(t)
				selectTarget(t, service)
				current := fake.currentAgent()
				fake.promptResult = current
				results := []agentGetResult{{agent: current}}
				for check := 1; check <= test.failureCheck; check++ {
					result := agentGetResult{agent: current}
					if check == test.failureCheck {
						switch {
						case test.readError:
							result.err = errors.New("private read failure")
						case test.sendError:
							fake.keyErr = errors.New("private key failure")
						case test.sessionChange:
							result.agent.AgentSession = &herdr.AgentSession{Source: "herdr:codex", Agent: "codex", Kind: "id", Value: "new-session"}
						default:
							result.agent.TerminalID = "replacement"
						}
					}
					results = append(results, result)
				}
				fake.setGetResults(results...)
				initialGets := len(fake.gets())
				started := time.Now()

				service.HandleMessage(context.Background(), incoming("recovery-failed", "继续执行"))

				if elapsed := time.Since(started); elapsed != promptCheckTime(test.failureCheck) || len(fake.gets())-initialGets != 1+test.failureCheck {
					t.Fatalf("失败后未停止：elapsed=%v gets=%#v", elapsed, fake.gets())
				}
				assertPromptRecoveryEffects(t, service, fake, test.wantKeys, test.wantAudit)
				want := unavailableMessage
				if !test.readError && !test.sendError {
					want = targetChangedMessage
					if test.failureCheck <= 4 {
						want = targetChangedAfterPromptMessage
					}
					if _, err := service.registry.ValidateSelected(); err == nil {
						t.Fatal("目标更换后未清除选择")
					}
				}
				if reply := fakeIMFromService(t, service).lastReply(); reply != want {
					t.Fatalf("恢复失败回复 = %q，期望 %q", reply, want)
				}
			})
		})
	}
}

func TestServicePromptDoesNotRetrySuccessOrOtherErrors(t *testing.T) {
	for _, promptErr := range []error{nil, errors.New("private prompt failure")} {
		synctest.Test(t, func(t *testing.T) {
			service, fake := newTestService(t)
			selectTarget(t, service)
			fake.promptErr = promptErr
			initialGets := len(fake.gets())
			started := time.Now()

			service.HandleMessage(context.Background(), incoming("no-retry", "继续执行"))

			if elapsed := time.Since(started); elapsed != 0 || len(fake.gets())-initialGets != 1 {
				t.Fatalf("不应进行状态复查：elapsed=%v gets=%#v", elapsed, fake.gets())
			}
			assertPromptSentOnceWithoutKeys(t, service, fake)
		})
	}
}

func assertPromptSentOnceWithoutKeys(t *testing.T, service *Service, fake *fakeHerdr) {
	t.Helper()
	assertPromptRecoveryEffects(t, service, fake, 0, "")
}

func assertPromptRecoveryEffects(t *testing.T, service *Service, fake *fakeHerdr, wantKeys int, wantAudit policy.AuditResult) {
	t.Helper()
	if prompts := fake.prompts(); len(prompts) != 1 {
		t.Fatalf("prompt 次数 = %d，期望只发送一次", len(prompts))
	}
	keys := fake.keys()
	if len(keys) != wantKeys {
		t.Fatalf("按键次数 = %d，期望 %d", len(keys), wantKeys)
	}
	if wantKeys == 1 && (keys[0].target != "pane-1" || keys[0].key != "enter") {
		t.Fatalf("恢复按键 = %#v", keys)
	}
	audits := fakeAuditFromService(t, service).records()
	if wantAudit == "" {
		if len(audits) != 0 {
			t.Fatalf("不应产生按键审计：%#v", audits)
		}
	} else if len(audits) != 1 || audits[0].Result() != wantAudit || audits[0].Key() != "enter" || audits[0].UserID() != "user-1" {
		t.Fatalf("恢复按键审计 = %#v，期望 %s", audits, wantAudit)
	}
}
