import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, chmod, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { readToken } from '../src/auth.mjs';

test('从私有文件加载 token，环境变量优先且不读取文件', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'dsh-pal-token-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, 'token'); const secret = 'a'.repeat(64);
  await writeFile(path, `${secret}\n`, { mode: 0o600 });
  assert.equal(await readToken({ tokenFile: path }, {}), secret);
  assert.equal(await readToken({ tokenFile: '/nonexistent' }, { DSH_PAL_TOKEN: secret }), secret);
  if (process.platform !== 'win32') {
    await chmod(path, 0o644);
    await assert.rejects(readToken({ tokenFile: path }, {}), /0600/);
  }
});

test('token 文件缺失或过大时给出不含凭据的错误', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'dsh-pal-token-'));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const path = join(dir, 'token');
  await assert.rejects(readToken({ tokenFile: path }, {}), /token/);
  await writeFile(path, 'sensitive'.repeat(100), { mode: 0o600 });
  await assert.rejects(readToken({ tokenFile: path }, {}), error => !error.message.includes('sensitive'));
});
