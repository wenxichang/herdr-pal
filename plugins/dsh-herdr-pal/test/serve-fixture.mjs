import { createBridge } from '../src/bridge.mjs';

// Go 集成测试使用真实 HTTP 插件，只有 dsh Agent 与持久化服务使用本地替身。
const messages = [
  { role: 'user', source: { kind: 'agent-instructions' }, content: [{ type: 'text', text: 'INTERNAL_INSTRUCTIONS' }] },
  { role: 'assistant', content: [{ type: 'reasoning', text: 'HIDDEN_REASONING' }, { type: 'text', text: '历史回答' }] },
];
const events = () => messages.map(message => ({ type: `${message.role}/message`, surfaceOp: 'append', data: message.role === 'assistant' ? { message } : message }));
let live;
const agent = { id: 'integration-session', status: 'idle', session: {
  header: { cwd: '/project/dsh-test' }, snapshotEvents: events,
}, followup(message) {
  messages.push({ ...message, role: 'user' });
  messages.push({ role: 'assistant', content: [{ type: 'text', text: `已完成：${message.content[0].text}` }] });
} };
const bridge = await createBridge({
  agents: { roots: () => live ? [live] : [], get: id => live?.id === id ? live : undefined },
  createUserMessage: x => x,
  sessionController: {
    list: async () => ({ items: [
      { sessionId: agent.id, cwd: '/project/dsh-test', updatedAt: Date.now(), projections: { values: { title: '两周内的历史会话' } } },
      { sessionId: 'old-session', updatedAt: Date.now() - 15 * 86400000 },
    ] }),
    resolveAgent: async () => { live = agent; return { agent }; },
  },
  sessionQuery: { readSession: async () => ({ events: events() }) },
}, { token: process.env.DSH_PAL_TOKEN, port: 0 });
console.log(bridge.url);
process.on('SIGTERM', async () => { await bridge.close(); process.exit(0); });
