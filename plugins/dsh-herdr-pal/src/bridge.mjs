import { createServer } from 'node:http';
import { createHash, randomUUID, timingSafeEqual } from 'node:crypto';
import { basename } from 'node:path';
import { conversationText } from './transcript.mjs';

const digest = value => createHash('sha256').update(value).digest('hex');
const fail = (status, code) => Object.assign(new Error(code), { status, code });

/** 清除终端控制序列，限制文本长度，避免向企微转发工具与推理内容。 */
export function sessionText(agent) {
  return conversationText(agent.session.snapshotEvents());
}

/** 启动仅供本机 Pal 使用的认证接口；返回地址及异步关闭函数。 */
export async function createBridge({ agents, createUserMessage, sessionController, sessionQuery }, { token, port = 17842, capacity = 4096 } = {}) {
  if (typeof token !== 'string' || !/^[\x21-\x7e]{32,256}$/.test(token)) throw new Error('DSH_PAL_TOKEN 必须为 32 到 256 个可打印 ASCII 字符');
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error('端口无效');
  const secret = Buffer.from(digest(token));
  const identities = new Map();
  const cache = new Map();
  const shutdown = new AbortController();
  const identity = (id, agent) => {
    let entry = identities.get(id);
    if (!entry || (entry.agent && entry.agent !== agent)) {
      entry = { incarnation: randomUUID(), agent }; identities.set(id, entry);
    }
    entry.agent = agent;
    return entry;
  };
  const list = async (signal) => {
    const rows = sessionController ? (await sessionController.list({}, signal)).items : agents.roots().map(a => ({ sessionId: a.id, cwd: a.session.header.cwd, updatedAt: Date.now() }));
    signal.throwIfAborted();
    const roots = new Map(agents.roots().map(a => [a.id, a]));
    const eligible = rows.filter(row => row.origin !== 'subagent' && row.formatStatus !== 'migration-required' &&
      (roots.has(row.sessionId) || row.updatedAt >= Date.now() - 14 * 86400000))
      .sort((a, b) => Number(roots.has(b.sessionId)) - Number(roots.has(a.sessionId)) || b.updatedAt - a.updatedAt).slice(0, 256);
    const ids = new Set(eligible.map(row => row.sessionId));
    for (const id of identities.keys()) if (!ids.has(id)) identities.delete(id);
    return eligible.map(row => {
      const agent = roots.get(row.sessionId);
      const state = identity(row.sessionId, agent);
      return { id: row.sessionId, incarnation: state.incarnation,
        title: row.projections?.values?.title || row.sessionId,
        workspace: basename(row.cwd || '') || 'dsh',
        status: agent && agent.status !== 'idle' ? 'working' : 'idle',
        revision: agent ? digest(sessionText(agent)) : String(row.updatedAt) };
    });
  };
  const resolve = async (request, signal) => {
    const row = (await list(signal)).find(row => row.id === request.id);
    if (!row) throw fail(404, 'session_not_found');
    if (row.incarnation !== request.incarnation) throw fail(409, 'session_changed');
    const agent = agents.roots().find(a => a.id === request.id);
    const entry = identities.get(request.id);
    if (entry?.incarnation !== request.incarnation || entry.agent !== agent) throw fail(409, 'session_changed');
    return { agent, entry };
  };
  const dispatch = async (path, request, signal) => {
    signal.throwIfAborted();
    if (path === '/v1/sessions') return { protocol: 1, sessions: await list(signal) };
    if (path === '/v1/read') {
      const { agent } = await resolve(request, signal);
      if (agent) return { text: sessionText(agent) };
      const history = await sessionQuery.readSession(request.id);
      return { text: conversationText(history.events) };
    }
    if (path !== '/v1/prompt') throw fail(404, 'method_not_found');
    if (typeof request.request_id !== 'string' || !request.request_id || request.request_id.length > 256 ||
        typeof request.text !== 'string' || !request.text.trim() || Buffer.byteLength(request.text) > 16384) throw fail(400, 'invalid_prompt');
    const resolved = await resolve(request, signal);
    let agent = resolved.agent;
    if (identities.get(request.id) !== resolved.entry) throw fail(409, 'session_changed');
    const signature = digest(JSON.stringify([request.id, request.incarnation, request.text]));
    const now = Date.now();
    for (const [key, entry] of cache) if (entry.expires <= now) cache.delete(key);
    const old = cache.get(request.request_id);
    if (old) {
      if (old.signature !== signature) throw fail(409, 'idempotency_conflict');
      if (!old.accepted) throw fail(409, 'indeterminate');
      return { accepted: true };
    }
    if (agent && agent.status !== 'idle') throw fail(409, 'agent_busy');
    if (cache.size >= capacity) throw fail(429, 'idempotency_full');
    const entry = { signature, expires: now + 600000, accepted: false };
    cache.set(request.request_id, entry);
    if (!agent) {
      const result = await sessionController.resolveAgent(request.id);
      if (result.error || !result.agent) throw fail(409, 'resume_failed');
      agent = result.agent;
      if (identities.get(request.id) !== resolved.entry || (resolved.entry.agent && resolved.entry.agent !== agent)) throw fail(409, 'session_changed');
      resolved.entry.agent = agent;
    }
    signal.throwIfAborted();
    if (identities.get(request.id) !== resolved.entry || agents.get(request.id) !== agent || agent.status !== 'idle') throw fail(409, 'session_changed');
    agent.followup(createUserMessage({ content: [{ type: 'text', text: request.text }], source: { kind: 'user' } }));
    entry.accepted = true;
    return { accepted: true };
  };
  const server = createServer(async (req, res) => {
    const disconnected = new AbortController();
    res.once('close', () => { if (!res.writableEnded) disconnected.abort(); });
    const signal = AbortSignal.any([shutdown.signal, disconnected.signal, AbortSignal.timeout(4500)]);
    try {
      if (req.headers.origin !== undefined) throw fail(403, 'browser_forbidden');
      const authorization = req.headers.authorization || '';
      if (!authorization.startsWith('Bearer ') || !timingSafeEqual(Buffer.from(digest(authorization.slice(7))), secret)) throw fail(401, 'unauthorized');
      if (req.method !== 'POST') throw fail(405, 'method_not_allowed');
      if (req.headers['content-type'] !== 'application/json') throw fail(415, 'json_required');
      let size = 0; const chunks = [];
      for await (const chunk of req) {
        size += chunk.length;
        if (size > 65536) throw fail(413, 'request_too_large');
        chunks.push(chunk);
      }
      let request;
      try { request = JSON.parse(Buffer.concat(chunks).toString('utf8')); } catch { throw fail(400, 'invalid_json'); }
      if (!request || Array.isArray(request) || typeof request !== 'object') throw fail(400, 'invalid_request');
      // 输入幂等记录在恢复 Agent 之前同步占位；慢恢复不占用目录和读取通道。
      const result = await dispatch(req.url, request, signal);
      res.writeHead(200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      res.end(JSON.stringify(result));
    } catch (error) {
      res.writeHead(error.status || 500, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
      res.end(JSON.stringify({ error: error.code || 'internal_error' }));
    }
  });
  server.requestTimeout = 5000;
  server.headersTimeout = 5000;
  server.maxConnections = 16;
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', resolve);
  });
  return { url: `http://127.0.0.1:${server.address().port}`, close: () => new Promise(resolve => {
    shutdown.abort(); server.close(resolve); server.closeAllConnections(); cache.clear();
  }) };
}
