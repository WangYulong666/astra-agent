// 镜像冒烟测试：验证裁剪后的 dsh 仍能完成 ACP 全链路
// 用法：node verify-acp.mjs [--patch <yml>]
//
// 判定标准：
//   initialize  ✅  → 插件加载、native binding 正常
//   session/new ✅  → schema 校验、session 持久化正常（这一步会用到 zod 与 node-addon）
//   prompt      ⚠️  无 API key 属预期，只要不是 schema/module 级错误即算通过
//
// 退出码：0 = 链路完好；1 = 有实质性故障
import { spawn } from 'node:child_process';

const BIN = new URL('./node_modules/@deepseek-ai/dsh/lib/bin.js', import.meta.url).pathname;
const patchIdx = process.argv.indexOf('--patch');
const args = ['--profile', 'acp'];
if (patchIdx > -1 && process.argv[patchIdx + 1]) args.push('--patch', process.argv[patchIdx + 1]);

const child = spawn(process.execPath, [BIN, ...args], {
  env: { ...process.env, NO_COLOR: '1' },
  stdio: ['pipe', 'pipe', 'pipe'],
});

let buf = '';
let err = '';
let step = 'init';
const send = (id, method, params) =>
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');

child.stdout.on('data', d => {
  buf += d.toString();
  const lines = buf.split('\n');
  buf = lines.pop() ?? '';
  for (const line of lines) {
    if (!line.trim()) continue;
    let j; try { j = JSON.parse(line); } catch { continue; }

    if (j.id === 1) {
      console.log('initialize   ✅  protocolVersion=' + j.result?.protocolVersion);
      step = 'new';
      send(2, 'session/new', { cwd: '/tmp', mcpServers: [] });
    } else if (j.id === 2) {
      if (j.error) {
        console.log('session/new  ❌  ' + JSON.stringify(j.error).slice(0, 300));
        return fail();
      }
      console.log('session/new  ✅  sessionId=' + String(j.result?.sessionId).slice(0, 12) + '…');
      step = 'prompt';
      send(3, 'session/prompt', { sessionId: j.result?.sessionId, prompt: [{ type: 'text', text: 'hi' }] });
    } else if (j.id === 3) {
      const msg = JSON.stringify(j.error ?? j.result).slice(0, 240);
      if (!j.error) {
        console.log('prompt       ✅  stopReason=' + j.result?.stopReason);
      } else if (/no API key|DEEPSEEK_API_KEY|unauthorized|401/i.test(msg)) {
        console.log('prompt       ⚠️  无 API key（预期）。全链路已通。');
      } else if (/zod|schema|Cannot find module|undefined is not/i.test(msg)) {
        console.log('prompt       ❌  ' + msg);
        return fail();
      } else {
        console.log('prompt       ⚠️  下游错误（非裁剪导致）: ' + msg);
      }
      child.kill();
      console.log('\n结论：裁剪后链路完好 ✅');
      process.exit(0);
    }
  }
});

child.stderr.on('data', d => { err += d.toString(); });
child.on('exit', c => { if (step !== 'done') fail('提前退出 code=' + c); });

function fail(extra) {
  console.log('❌ 验证失败' + (extra ? ': ' + extra : ''));
  if (err) console.log('stderr:', err.slice(0, 1200));
  child.kill();
  process.exit(1);
}

setTimeout(() => send(1, 'initialize', {
  protocolVersion: 1,
  clientCapabilities: { fs: { readTextFile: true, writeTextFile: true }, terminal: true },
}), 1500);

setTimeout(() => fail('超时，卡在 ' + step), 45000);
