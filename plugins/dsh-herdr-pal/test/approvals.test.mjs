import test from 'node:test';
import assert from 'node:assert/strict';
import { createBridge } from '../src/bridge.mjs';
import { createApprovalBroker } from '../src/approvals.mjs';

const token = 'a'.repeat(48);

const request = (extra = {}) => ({ agent: { id: 'session-1' }, toolName: 'plugin_manager', ...extra });

test('本地先答就用本地结果，远端条目随之失效', async () => {
  const broker = createApprovalBroker();
  const outcome = await broker.request(request(), async () => 'allowed-once');
  assert.equal(outcome, 'allowed-once');
  assert.deepEqual(broker.list(), []);
});

test('远端先答就用远端结果，本地链路照常启动', async () => {
  const broker = createApprovalBroker();
  let localStarted = false;
  const pending = broker.request(request({ reason: '改 bundle' }), async () => {
    localStarted = true;
    await new Promise(resolve => setTimeout(resolve, 50));
    return 'rejected';
  });
  assert.equal(localStarted, true, 'next() 必须被立即调用，浏览器卡片才会出现');
  const [entry] = broker.list();
  assert.equal(entry.reason, '改 bundle');
  assert.equal(broker.answer(entry.id, 'allow'), true);
  assert.equal(await pending, 'allowed-once');
  assert.deepEqual(broker.list(), []);
  assert.equal(broker.answer(entry.id, 'deny'), false, '同一条审批不能被答复两次');
});

test('本地无人接时保留远端通道，超过兜底时间才失败关闭', async () => {
  const broker = createApprovalBroker({ timeoutMs: 1000 });
  const started = Date.now();
  const outcome = await broker.request(request(), async () => 'unavailable');
  assert.equal(outcome, 'unavailable');
  assert.ok(Date.now() - started >= 900, '应当等待兜底时间而不是立即失败关闭');
});

test('取消会同时结束两个通道', async () => {
  const broker = createApprovalBroker();
  const controller = new AbortController();
  const pending = broker.request(request({ signal: controller.signal }), async () => {
    await new Promise(resolve => setTimeout(resolve, 5000));
    return 'rejected';
  });
  controller.abort();
  assert.equal(await pending, 'cancelled');
  assert.deepEqual(broker.list(), []);
});

test('接口：查询与答复都走同一份待审列表', async t => {
  const broker = createApprovalBroker();
  const bridge = await createBridge({ agents: { roots: () => [] } }, { token, port: 0, broker });
  t.after(() => bridge.close());
  const call = async (path, body) => {
    const response = await fetch(`${bridge.url}/v1/${path}`, {
      method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    return { status: response.status, body: await response.json() };
  };
  assert.deepEqual((await call('approvals', {})).body, { protocol: 1, approvals: [] });
  assert.equal((await call('approvals/answer', { id: 'missing', decision: 'allow' })).status, 404);
  assert.equal((await call('approvals/answer', { id: 'x', decision: 'maybe' })).status, 400);

  const pending = broker.request(request(), async () => 'unavailable');
  const [{ id }] = (await call('approvals', {})).body.approvals;
  assert.equal((await call('approvals/answer', { id, decision: 'deny' })).body.accepted, true);
  assert.equal(await pending, 'rejected');
});
