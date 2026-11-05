/** 从原始会话事件提取聊天正文；不使用模型压缩后的上下文视图。 */
export function conversationText(events) {
  const messages = [];
  for (let index = events.length - 1; index >= 0 && messages.length < 100; index--) {
    const event = events[index];
    if (event.surfaceOp !== 'append') continue;
    let role;
    let content;
    if (event.type === 'user/message' && event.data.source?.kind === 'user') {
      role = '用户'; content = event.data.content;
    } else if (event.type === 'assistant/message') {
      role = '助手'; content = event.data.message.content;
    } else continue;
    const text = content.filter(block => block.type === 'text').map(block => block.text).join('\n');
    if (text) messages.push(`${role}：${text}`);
  }
  return messages.reverse().join('\n\n')
    .replace(/\x1b\][^\x07]*?(?:\x07|\x1b\\)/g, '')
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '')
    .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, '').slice(-16000);
}
