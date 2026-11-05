import { createUserMessage } from '@deepseek-ai/dsh-llm';
import { createBridge } from './bridge.mjs';
import { readToken } from './auth.mjs';

export const name = 'dsh-herdr-pal';
export const inject = ['agents', 'sessionController', 'sessionQuery'];

/** 通过 Cordis 生命周期管理本地桥接服务，不持有企微凭据。 */
export async function apply(ctx, config = {}) {
  await ctx.effect(async () => {
    const bridge = await createBridge({ agents: ctx.agents, createUserMessage, sessionController: ctx.sessionController, sessionQuery: ctx.sessionQuery }, {
      token: await readToken(config), port: config.port ?? 17842,
    });
    return () => bridge.close();
  });
}
