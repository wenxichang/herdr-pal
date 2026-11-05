import test from 'node:test';
import assert from 'node:assert/strict';
import { sessionText } from '../src/bridge.mjs';

const text = value => ({ type: 'text', text: value });
const user = (value, kind = 'user') => ({ role: 'user', source: { kind }, content: [text(value)] });
const assistant = content => ({ role: 'assistant', source: { kind: 'model' }, content });
const event = message => ({ type: `${message.role}/message`, surfaceOp: 'append', data: message.role === 'assistant' ? { message } : message });
const render = (events, modelMessages = events.map(e => e.type === 'assistant/message' ? e.data.message : e.data)) => sessionText({ session: {
  snapshotEvents: () => events, deriveMessages: () => modelMessages,
} });

test('聊天文本只保留真实用户输入和助手正文', () => {
  const events = [
    event(user('第一轮提问')),
    event(assistant([text('第一轮回答')])),
    event(user('INTERNAL_INSTRUCTIONS', 'agent-instructions')),
    event(user('INTERNAL_CLOCK', 'time-context')),
    event(user('INTERNAL_GOAL', 'goal-round')),
    event(user('第二轮提问')),
    event(assistant([{ type: 'reasoning', text: 'HIDDEN_REASONING' }, text('第二轮回答'), { type: 'tool_call', name: 'bash', arguments: 'PRIVATE_COMMAND' }])),
    { type: 'tool/result', surfaceOp: 'append', data: { message: { role: 'tool', content: [text('PRIVATE_RESULT')] } } },
  ];
  assert.equal(render(events), '用户：第一轮提问\n\n助手：第一轮回答\n\n用户：第二轮提问\n\n助手：第二轮回答');
});

test('压缩模型上下文后仍展示原始聊天，不展示替换摘要', () => {
  const summary = user('INTERNAL_COMPACTION', 'compact-checkpoint');
  const events = [event(user('压缩前的提问')), event(assistant([text('压缩前的回答')])),
    { ...event(summary), surfaceOp: { op: 'replace', startSeq: 0, endSeq: 1 } }];
  assert.equal(render(events, [summary]), '用户：压缩前的提问\n\n助手：压缩前的回答');
});

test('内部事件不占用最近一百条可见消息的名额', () => {
  const events = [event(user('不能被内部事件挤掉')), event(assistant([text('可见回答')]))];
  for (let i = 0; i < 150; i++) events.push(event(user(`INTERNAL_${i}`, 'time-context')));
  assert.equal(render(events), '用户：不能被内部事件挤掉\n\n助手：可见回答');
});

test('按来源过滤而不删改用户正文，不把未知来源当用户', () => {
  const events = [event({ role: 'user', content: [text('UNKNOWN_SOURCE')] }), event(user('请解释 <think> 和 tool_call 文本'))];
  assert.equal(render(events), '用户：请解释 <think> 和 tool_call 文本');
});

test('多个 OSC 8 超链接保留标签与中间正文', () => {
  for (const end of ['\x1b\\', '\x07']) {
    const link = (url, label) => `\x1b]8;;${url}${end}${label}\x1b]8;;${end}`;
    const value = `开头 ${link('https://example.com/a', '文档链接')} 中间正文 ${link('https://example.com/b', '另一个链接')} 结尾`;
    assert.equal(render([event(assistant([text(value)]))]), '助手：开头 文档链接 中间正文 另一个链接 结尾');
  }
});
