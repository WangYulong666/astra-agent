# dsh 裁剪方案（DeepSeek Harness → ACP headless 执行器）

针对 `@deepseek-ai/dsh@0.2.1-rc.2`。目标：在 CubeSandbox 里跑 ACP headless，砍掉一切与 agent 执行无关的东西。

## 结论

| 指标 | 值 |
|---|---|
| 全量安装（linux/x64，614 包） | **826MB** |
| + zod overrides 去重 | **511MB**（−315MB） |
| + 七项功能裁剪 | **138MB**（−373MB） |
| **压缩后（≈ docker 层增量）** | **25MB** |
| 最终 | 187 个包 / 11645 个文件 |

总计 **826MB → 138MB 落盘，压缩层 25MB**，减幅 83%。

功能验证：`initialize` ✅ → `session/new` ✅（触发 schema 校验与 session 持久化）→ `prompt` 仅因无 API key 停止。链路完好。

## 两个压倒性的收益点

### 1. zod 重复副本：315MB（比所有功能裁剪加起来还多）

46 个 dsh 插件对 `zod` 的声明是**精确版本 `4.4.3`**，而顶层安装的是 `4.6.7`（`@modelcontextprotocol/core` 等 6 处 range 声明取最新）。npm 对精确版本冲突的处理是给每个包嵌入一份副本：

```
node_modules/@deepseek-ai/dsh-tool-todo/node_modules/zod   7MB
node_modules/@deepseek-ai/dsh-tool-workspace/node_modules/zod  7MB
… × 46 份 = 322MB
```

解法：`package.json` 里 `"overrides": { "zod": "4.4.3" }`。4.4.3 满足全部 6 处 range（`^4.4.3`、`^4.2.0`、`^3.25.0 || ^4.0.0`），也满足 46 处精确要求。verify 脚本跑通说明 API 无差异。

### 2. 功能裁剪：373MB

| 步骤 | 省 | 删除内容 | 理由 |
|---|---|---|---|
| ① | −146MB | `libreoffice-kit-wasm` | Office 转换（`soffice.wasm` 122MB）。沙箱不跑 LibreOffice |
| ② | −32MB | `sherpa-onnx-*` | 语音 ASR/TTS（含 onnxruntime 26MB）。无音频输入 |
| ③ | −8MB | `@img/sharp-wasm32` | 图片处理 wasm 兜底，native sharp 已够 |
| ④ | −28MB | `@opentelemetry/*` + `dsh-otel` | **官方遥测，必须关**（见下） |
| ⑤ | −73MB | `pi-ai` `openai` `@anthropic-ai` `@google/genai` `aws-sdk` `smithy` | 非 DeepSeek provider |
| ⑥ | −63MB | `dsh-client-ui-*` `dsh-experimental-*` `dsh-web-app` | ACP 是 headless，不渲染 UI |
| ⑦ | −24MB | node-pty win32/darwin prebuilds、`*.pdb` | 跨平台二进制，镜像只需 linux-x64 |

## 必须知道的三个事实

### ① 遥测默认外发，不能只当体积问题处理

`dsh-base/cordis.patch.yml` 里 otel 插件的默认 endpoint 是：

```
https://dsh-otel-collector.deepseeksvc.com/v1/logs
```

也就是**默认把沙箱内的 trace 发到 DeepSeek 官方收集端**。删包 + `disabled: true` 是合规项，不是优化项。

### ② npm 的平台过滤只在「一次性安装」时可靠

`npm install --os=linux --cpu=x64` 装出来的确实是干净的（无 darwin 平台包）。但只要事后单独 `npm i <任何包>`，npm 会按**当前构建机平台**重算整棵树的 optionalDependencies，一次性把 `sharp-darwin-arm64`、`sherpa-onnx-darwin-arm64`、`koffi-darwin-arm64`、`ripgrep-darwin-arm64` 全拉回来（+125MB）。

⇒ Dockerfile 里必须一次装完，禁止后续 `npm i`。补装任何东西都要走 `npm pack` + 手工解包。

### ③ patch 是运行契约，不是可选优化

三个被禁用的插件（`llm-pi-ai`、`otel`、`session-telemetry-otel`）原本是懒加载 —— 不传 `--patch` 也能握手成功，因为初始化阶段不触发它们。但一旦：

- 配置了非 DeepSeek provider → `llm-pi-ai` 被调用 → 模块已删 → 崩
- 触发遥测上报 → `dsh-otel` 被调用 → 崩

所以启动命令必须固定带 `--patch /opt/dsh/trim-patch.yml`。

## 能力边界（裁剪前就存在，非裁剪导致）

实测 `initialize` 返回的 `agentCapabilities`：

```json
{
  "promptCapabilities": { "image": false, "audio": false, "embeddedContext": false },
  "mcpCapabilities": { "http": true },
  "sessionCapabilities": { "close": {}, "list": {}, "resume": {} }
}
```

对比结论：

- **不支持图片输入**（`image: false`）→ 体检平台若要让 agent 看图，得自己在 MCP 工具里包一层（agent 可调 tool 读文件），不能走 ACP 原生 image block
- **不支持 `embeddedContext`** → 附件不能内嵌进 prompt，只能落盘让 agent 读
- **无 `loadSession`**，但有 `resume` / `list` → 断线恢复靠 `session/resume`，不是回放
- MCP 只支持 `http`，**没有 sse**

这些是 dsh 的 ACP 实现选择，裁不裁剪都一样。端侧如果依赖图片/嵌入上下文，需要提前设计绕行方案。

## 文件

| 文件 | 用途 |
|---|---|
| `Dockerfile` | 两阶段构建，build 阶段装+裁，运行阶段拷 `node_modules` |
| `package.json` | 钉住 dsh 版本 + zod overrides（**关键**） |
| `trim.sh` | 七项功能裁剪，可独立执行、幂等 |
| `trim-patch.yml` | 关闭 llm-pi-ai / otel / session-telemetry-otel |
| `verify-acp.mjs` | 冒烟测试：initialize → session/new → prompt，退出码即结论 |

## 使用

```bash
# 构建
docker build -t astra/dsh-agent:0.2.1-rc.2 .

# 冒烟测试（无需 API key，验证链路）
docker run --rm astra/dsh-agent:0.2.1-rc.2 \
  node /opt/dsh/verify-acp.mjs --patch /opt/dsh/trim-patch.yml

# 实际运行（API key 由沙箱注入）
docker run --rm -i -e DEEPSEEK_API_KEY=sk-xxx astra/dsh-agent:0.2.1-rc.2
```

⚠️ 基础镜像必须用 **glibc 系**（`node:22-bookworm-slim`）。dsh 的 native binding 与 node-pty 的 prebuild 都是 `linux-x64-gnu`，**alpine/musl 跑不了**。

## 还能再压吗

当前 138MB 里，`@deepseek-ai` 占约 110MB、共 150+ 个插件包（多为 1–3MB 的 JS）。剩余可压空间：

- `*.d.ts`（7101 个）+ `*.ts`（20768 个）合计约 3MB —— 体积收益小，但**文件数减半对镜像层和冷启动有实际意义**
- `docs` / `test` / `examples` / `.github` 目录约 5MB
- 进一步砍插件：`dsh-tool-pwsh*`（PowerShell，linux 无用）、`dsh-skill-*`、`dsh-tool-workflow` 等，但**每砍一个都要重跑 verify**，边际收益递减

不建议继续压到 100MB 以下 —— 剩下的都是 agent 真实会用到的执行插件，砍错一个就是运行时才暴露的坑。
