import { open } from 'node:fs/promises';
import { constants } from 'node:fs';

/** 环境变量优先，否则从仅当前用户可读的普通文件读取本机连接 token。 */
export async function readToken(config = {}, env = process.env) {
  if (env.DSH_PAL_TOKEN !== undefined) return env.DSH_PAL_TOKEN;
  if (!config.tokenFile) throw new Error('请设置 DSH_PAL_TOKEN 或插件 tokenFile');
  let file;
  try {
    file = await open(config.tokenFile, constants.O_RDONLY | (constants.O_NOFOLLOW || 0));
    const stat = await file.stat();
    if (!stat.isFile() || stat.size > 258) throw new Error('token 文件类型或大小无效');
    if (process.platform !== 'win32' && (stat.mode & 0o077)) throw new Error('token 文件权限必须为 0600');
    const buffer = Buffer.alloc(259);
    const { bytesRead } = await file.read(buffer, 0, buffer.length, 0);
    if (bytesRead > 258) throw new Error('token 文件过大');
    return buffer.subarray(0, bytesRead).toString('utf8').trim();
  } catch (error) {
    if (error.message === 'token 文件权限必须为 0600') throw error;
    throw new Error('无法读取有效 token 文件');
  } finally { await file?.close(); }
}
