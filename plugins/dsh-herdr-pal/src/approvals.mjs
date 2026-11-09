import { randomUUID } from 'node:crypto';

/**
 * 审批竞速器：本地 answerer 与远端（Pal → 企业微信）同时持有同一条审批，
 * 谁先作出决定就用谁的结果，任何一方都不会被吞掉。
 *
 * - 本地链路始终先跑：浏览器卡片照常出现，插件不接管；
 * - 远端条目只做“可查询/可答复”的副本，不阻塞本地；
 * - 本地返回 'unavailable'（没有浏览器在等）时才启动兜底计时，避免两边都没人时永久挂起。
 */
export function createApprovalBroker({ timeoutMs = 1800000, capacity = 64, now = Date.now } = {}) {
  const pending = new Map();

  const forget = (id) => {
    const entry = pending.get(id);
    if (entry === undefined) return;
    pending.delete(id);
    if (entry.timer !== undefined) clearTimeout(entry.timer);
  };

  const answer = (id, decision) => {
    const entry = pending.get(id);
    if (entry === undefined) return false;
    forget(id);
    entry.resolve(decision === 'allow' ? 'allowed-once' : 'rejected');
    return true;
  };

  const request = (req, next, { remote = true } = {}) => {
    // 先让本地链路接管显示：同步调用 next()，浏览器卡片立即出现。
    let local;
    try {
      local = Promise.resolve(next()).catch(() => 'unavailable');
    } catch {
      local = Promise.resolve('unavailable');
    }
    if (!remote || pending.size >= capacity) return local;

    const id = randomUUID();
    const entry = {
      id, timer: undefined, resolve: () => {},
      sessionId: String(req.agent?.id ?? ''),
      toolName: String(req.toolName ?? ''),
      ...(req.callId === undefined ? {} : { callId: String(req.callId) }),
      ...(req.reason === undefined ? {} : { reason: String(req.reason) }),
      createdAt: new Date(now()).toISOString(),
    };
    pending.set(id, entry);

    return new Promise((resolve) => {
      entry.resolve = resolve;
      let settled = false;
      const finish = (outcome) => {
        if (settled) return;
        settled = true;
        forget(id);
        resolve(outcome);
      };
      // 本地给出真实决定（点击、取消）时立即采用；'unavailable' 不算决定，继续等远端。
      local.then(outcome => {
        if (outcome === 'unavailable') return;
        finish(outcome);
      });
      req.signal?.addEventListener('abort', () => finish('cancelled'), { once: true });
      // 本地没人接（没有浏览器在等）时，才给远端一个上限，避免永久挂起。
      local.then(outcome => {
        if (outcome !== 'unavailable' || settled) return;
        entry.timer = setTimeout(() => finish('unavailable'), Math.max(1000, timeoutMs));
        entry.timer.unref?.();
      });
      // 远端先答：覆盖本地尚未决定的等待。
      entry.resolve = finish;
    });
  };

  return {
    request,
    list: () => [...pending.values()].map(entry => ({
      id: entry.id,
      sessionId: entry.sessionId,
      toolName: entry.toolName,
      ...(entry.callId === undefined ? {} : { callId: entry.callId }),
      ...(entry.reason === undefined ? {} : { reason: entry.reason }),
      createdAt: entry.createdAt,
    })),
    answer,
    size: () => pending.size,
  };
}
