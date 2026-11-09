import { createUserMessage } from '@deepseek-ai/dsh-llm';
import { createBridge } from './bridge.mjs';
import { readToken } from './auth.mjs';
import { createApprovalBroker } from './approvals.mjs';

export const name = 'dsh-herdr-pal';
export const inject = ['agents', 'sessionController', 'sessionQuery'];

/**
 * 通过 Cordis 生命周期管理本地桥接服务；可选地把审批变成“两边都能答”的竞速通道。
 */
export async function apply(ctx, config = {}) {
  const remoteApproval = config.remoteApproval ?? {};
  const broker = createApprovalBroker({
    timeoutMs: Number.isFinite(remoteApproval.timeoutMs) ? remoteApproval.timeoutMs : 1800000,
  });
  if (remoteApproval.enabled === true) {
    // prepend 只影响顺序；request 内部先调用 next()，浏览器卡片照常出现，不接管。
    ctx.on('approval/request', (request, next) => broker.request(request, next), { prepend: true });
  }
  await ctx.effect(async () => {
    const bridge = await createBridge({ agents: ctx.agents, createUserMessage, sessionController: ctx.sessionController, sessionQuery: ctx.sessionQuery }, {
      token: await readToken(config), port: config.port ?? 17842, broker,
    });
    return () => bridge.close();
  });
}
