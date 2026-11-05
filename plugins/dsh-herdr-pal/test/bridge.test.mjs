import test from 'node:test';
import assert from 'node:assert/strict';
import { createBridge } from '../src/bridge.mjs';

const token = 'a'.repeat(48);
function fixture() {
  const messages = [];
  const agent = { id: 'session/中文', status: 'idle', session: {
    header: { cwd: '/workspace/demo' }, snapshotEvents: () => messages.map(message => ({
      type: `${message.role}/message`, surfaceOp: 'append', data: message.role === 'assistant' ? { message } : message,
    })),
  }, followup(message) { messages.push({ ...message, role: 'user' }); } };
  let current = agent;
  const agents = { roots: () => current ? [current] : [], get: id => current?.id === id ? current : undefined };
  return { agent, agents, messages, replace: value => { current = value; } };
}
async function start(t, fixture, options = {}) {
  const bridge = await createBridge({ agents: fixture.agents, createUserMessage: x => x }, { token, port: 0, ...options });
  t.after(() => bridge.close());
  const call = async (path, body, headers = {}) => {
    const response = await fetch(`${bridge.url}/v1/${path}`, {
      method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', ...headers }, body: JSON.stringify(body),
    });
    return { status: response.status, body: await response.json() };
  };
  return { bridge, call };
}
test('认证失败及浏览器请求不能读取会话', async t => {
  const { call } = await start(t, fixture());
  assert.equal((await call('sessions', {}, { Authorization: 'Bearer wrong' })).status, 401);
  assert.equal((await call('sessions', {}, { Origin: 'https://example.com' })).status, 403);
});
test('列表、读取和发送沿用同一 Agent，重复请求不重复输入', async t => {
  const f = fixture();
  f.messages.push({ role: 'assistant', content: [{ type: 'text', text: '\x1b[31m你好\x1b[0m' }] });
  const { call } = await start(t, f);
  const { body: { sessions } } = await call('sessions', {});
  assert.equal(sessions.length, 1);
  assert.equal(sessions[0].workspace, 'demo');
  const target = { id: sessions[0].id, incarnation: sessions[0].incarnation };
  assert.match((await call('read', target)).body.text, /你好/);
  assert.doesNotMatch((await call('read', target)).body.text, /\x1b/);
  const request = { ...target, request_id: 'unique-1', text: '执行测试' };
  assert.equal((await call('prompt', request)).status, 200);
  assert.equal((await call('prompt', request)).status, 200);
  assert.equal(f.messages.length, 2);
  assert.equal((await call('prompt', { ...request, text: '其他命令' })).status, 409);
});
test('忙碌、未知会话和同 ID 替换不会误发', async t => {
  const f = fixture(); const { call } = await start(t, f);
  const session = (await call('sessions', {})).body.sessions[0];
  const request = { id: session.id, incarnation: session.incarnation, request_id: 'unique-2', text: 'hello' };
  f.agent.status = 'running';
  assert.equal((await call('prompt', request)).status, 409);
  f.replace({ ...f.agent, status: 'idle' });
  assert.equal((await call('prompt', request)).status, 409);
  f.replace(null);
  assert.equal((await call('read', request)).status, 404);
  assert.equal(f.messages.length, 0);
});
test('幂等容量满时拒绝新输入，保留旧键', async t => {
  const f = fixture(); const { call } = await start(t, f, { capacity: 1 });
  const session = (await call('sessions', {})).body.sessions[0];
  const request = { id: session.id, incarnation: session.incarnation, request_id: 'one', text: 'hello' };
  assert.equal((await call('prompt', request)).status, 200);
  assert.equal((await call('prompt', { ...request, request_id: 'two' })).status, 429);
  assert.equal((await call('prompt', request)).status, 200);
  assert.equal(f.messages.length, 1);
});
test('拒绝短 token、过大消息及未知方法', async t => {
  await assert.rejects(createBridge({ agents: fixture().agents }, { token: 'short' }));
  const { call } = await start(t, fixture());
  assert.equal((await call('prompt', { text: 'x'.repeat(100000) })).status, 413);
  assert.equal((await call('approve', {})).status, 404);
});

test('历史只列两周内，会话读取不恢复，发消息按需恢复且身份不变', async t => {
  const f = fixture(); f.replace(null); let resumes = 0;
  const controller = {
    list: async () => ({ items: [
      { sessionId: f.agent.id, updatedAt: Date.now(), cwd: '/project/demo', running: false },
      { sessionId: 'expired', updatedAt: Date.now() - 15 * 86400000 },
    ] }),
    resolveAgent: async id => { assert.equal(id, f.agent.id); resumes++; f.replace(f.agent); return { agent: f.agent }; },
  };
  const query = { readSession: async () => ({ events: [
    { type: 'user/message', surfaceOp: 'append', data: { source: { kind: 'time-context' }, content: [{ type: 'text', text: 'INTERNAL_CLOCK' }] } },
    { type: 'assistant/message', surfaceOp: 'append', data: { message: { content: [{ type: 'reasoning', text: 'HIDDEN_REASONING' }, { type: 'text', text: '历史回答' }] } } },
  ] }) };
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x, sessionController: controller, sessionQuery: query }, { token, port: 0 });
  t.after(() => bridge.close());
  const call = async (path, body = {}) => {
    const r = await fetch(`${bridge.url}/v1/${path}`, { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    assert.equal(r.status, 200); return r.json();
  };
  const sessions = (await call('sessions')).sessions; assert.equal(sessions.length, 1);
  const target = { id: sessions[0].id, incarnation: sessions[0].incarnation };
  assert.equal((await call('read', target)).text, '助手：历史回答'); assert.equal(resumes, 0);
  const request = { ...target, request_id: 'restore-1', text: '继续' };
  await call('prompt', request); await call('prompt', request);
  assert.equal(resumes, 1); assert.equal(f.messages.length, 1);
  assert.equal((await call('sessions')).sessions[0].incarnation, target.incarnation);
});

test('恢复期间请求取消后不再提交输入', async t => {
  const f = fixture(); f.replace(null); let release; let started;
  const pending = new Promise(resolve => { release = resolve; });
  const entered = new Promise(resolve => { started = resolve; });
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x,
    sessionController: {
      list: async () => ({ items: [{ sessionId: f.agent.id, updatedAt: Date.now() }] }),
      resolveAgent: async () => { started(); await pending; f.replace(f.agent); return { agent: f.agent }; },
    },
  }, { token, port: 0 });
  t.after(() => bridge.close());
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  const list = await fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers, body: '{}' });
  const session = (await list.json()).sessions[0];
  const controller = new AbortController();
  const request = fetch(`${bridge.url}/v1/prompt`, { method: 'POST', headers, signal: controller.signal,
    body: JSON.stringify({ id: session.id, incarnation: session.incarnation, text: '不能迟到提交', request_id: 'cancel' }) }).catch(() => {});
  await entered; controller.abort(); await request;
  await new Promise(resolve => setTimeout(resolve, 30)); release();
  await new Promise(resolve => setTimeout(resolve, 30));
  assert.equal(f.messages.length, 0);
});

test('两周时间边界包含当天，已加载老会话保留，子 Agent 排除', async t => {
  const now = Date.now(); t.mock.method(Date, 'now', () => now);
  const f = fixture();
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x, sessionController: {
    list: async () => ({ items: [
      { sessionId: 'boundary', updatedAt: now - 14 * 86400000 },
      { sessionId: 'expired', updatedAt: now - 14 * 86400000 - 1 },
      { sessionId: f.agent.id, updatedAt: 0 },
      { sessionId: 'sub', updatedAt: now, origin: 'subagent' },
      { sessionId: 'migration', updatedAt: now, formatStatus: 'migration-required' },
    ] }),
  } }, { token, port: 0 });
  t.after(() => bridge.close());
  const response = await fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: '{}' });
  assert.deepEqual((await response.json()).sessions.map(s => s.id), [f.agent.id, 'boundary']);
});

test('并发重放只提交一次，重启后旧 incarnation 失效', async t => {
  const f = fixture(); const first = await start(t, f);
  const session = (await first.call('sessions', {})).body.sessions[0];
  const request = { id: session.id, incarnation: session.incarnation, request_id: 'parallel', text: '一次' };
  const results = await Promise.all(Array.from({ length: 8 }, () => first.call('prompt', request)));
  assert.ok(results.every(r => r.status === 200)); assert.equal(f.messages.length, 1);
  await first.bridge.close();
  const second = await start(t, f);
  assert.equal((await second.call('prompt', request)).status, 409);
  assert.equal(f.messages.length, 1);
});

test('列表等待期间替换同 ID Agent，旧选择不能向新实例发消息', async t => {
  const f = fixture(); let replace = false; let sent = 0;
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x, sessionController: {
    list: async () => {
      if (replace) { f.replace({ ...f.agent, followup() { sent++; } }); replace = false; }
      return { items: [{ sessionId: f.agent.id, updatedAt: Date.now() }] };
    },
  } }, { token, port: 0 });
  t.after(() => bridge.close());
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  const first = await fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers, body: '{}' });
  const session = (await first.json()).sessions[0]; replace = true;
  const response = await fetch(`${bridge.url}/v1/prompt`, { method: 'POST', headers,
    body: JSON.stringify({ id: session.id, incarnation: session.incarnation, request_id: 'replacement', text: '不能误投' }) });
  assert.equal(response.status, 409); assert.equal(sent, 0);
});

test('历史恢复等待期间仍能读取其他会话目录', async t => {
  const f = fixture(); f.replace(null); let release; let started;
  const pending = new Promise(resolve => { release = resolve; });
  const entered = new Promise(resolve => { started = resolve; });
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x, sessionController: {
    list: async () => ({ items: [{ sessionId: f.agent.id, updatedAt: Date.now() }] }),
    resolveAgent: async () => { started(); await pending; f.replace(f.agent); return { agent: f.agent }; },
  } }, { token, port: 0 });
  t.after(() => bridge.close());
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  const first = await fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers, body: '{}' });
  const session = (await first.json()).sessions[0];
  const prompt = fetch(`${bridge.url}/v1/prompt`, { method: 'POST', headers,
    body: JSON.stringify({ id: session.id, incarnation: session.incarnation, request_id: 'slow', text: '继续' }) });
  await entered;
  try {
    const response = await fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers, body: '{}', signal: AbortSignal.timeout(1000) });
    assert.equal(response.status, 200);
  } finally { release(); await prompt; }
});

test('并发目录刷新不能用新身份覆盖正在校验的旧请求', async t => {
  const f = fixture(); let concurrent = false; let replace = false; let sent = 0;
  const pending = []; const originalRoots = f.agents.roots;
  f.agents.roots = () => {
    if (replace) { f.replace({ ...f.agent, followup() { sent++; } }); replace = false; }
    return originalRoots();
  };
  const bridge = await createBridge({ agents: f.agents, createUserMessage: x => x, sessionController: {
    list: () => {
      const result = { items: [{ sessionId: f.agent.id, updatedAt: Date.now() }] };
      return concurrent ? new Promise(resolve => pending.push(() => resolve(result))) : Promise.resolve(result);
    },
  } }, { token, port: 0 });
  t.after(() => bridge.close());
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  const getList = () => fetch(`${bridge.url}/v1/sessions`, { method: 'POST', headers, body: '{}' });
  const session = (await (await getList()).json()).sessions[0]; concurrent = true;
  const prompt = fetch(`${bridge.url}/v1/prompt`, { method: 'POST', headers,
    body: JSON.stringify({ id: session.id, incarnation: session.incarnation, request_id: 'parallel-replace', text: '旧选择' }) });
  while (pending.length < 1) await new Promise(resolve => setTimeout(resolve, 1));
  const list = getList();
  while (pending.length < 2) await new Promise(resolve => setTimeout(resolve, 1));
  pending[0](); queueMicrotask(() => { replace = true; }); pending[1]();
  assert.equal((await prompt).status, 409); await list; assert.equal(sent, 0);
});
