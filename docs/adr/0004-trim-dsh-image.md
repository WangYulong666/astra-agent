# 0004. 用 npm overrides 裁剪 dsh 镜像

- **状态**：已采纳
- **日期**：2026-10-28
- **相关**：ADR-0003（选 dsh）、`dsh-trim/README.md`（可复现脚本与实测数据）

## 背景

`@deepseek-ai/dsh` 在 linux/x64 上完整安装是 **826MB / 614 个包**。作为沙箱内的 agent 镜像，这个体积会拉长模板拉取和冷启动，也压节点磁盘。

## 决策

**主手段是 npm `overrides` 做依赖去重，不是删包。**

```json
{ "overrides": { "zod": "4.4.3" } }
```

### 为什么这一行收益最大

46 个 dsh 插件对 `zod` 声明的是**精确版本 `4.4.3`**，而顶层被几个 MCP 包按 range 解析到了 `4.6.7`。npm 处理精确版本冲突的方式是**给每个插件嵌一份副本**：

```
dsh-tool-todo/node_modules/zod          7MB
dsh-tool-workspace/node_modules/zod     7MB
… × 46 份 = 322MB
```

把顶层对齐到 `4.4.3`（满足全部 6 处 range 声明）后，副本降到 0，**省 315MB** —— 比其余六项功能裁剪加起来还接近。

### 叠加的功能裁剪（共省 373MB）

| 项 | 收益 | 依据 |
|---|---|---|
| Office wasm kit | −146MB | ACP profile 不加载 |
| sherpa-onnx 语音 | −32MB | 同上 |
| sharp-wasm32 | −8MB | 图像处理，agent 用不到 |
| OpenTelemetry | −28MB | 见下「遥测」 |
| 多厂商 LLM SDK（openai / anthropic / google / aws） | −73MB | 用 patch 禁用 `llm-pi-ai` |
| Web UI + experimental profile（62 个 client-ui 包） | −63MB | 只被 web profile 引用 |
| 跨平台二进制（win32/darwin prebuilds） | −24MB | 沙箱只要 linux |

结果：**826MB → 138MB**，压缩层（≈镜像仓库与拉取看到的数字）**25MB**，187 个包。

## 三条必须知道的事实

### 1. 遥测默认外发 —— 这是合规项，不是优化项

`dsh-otel` 的默认 endpoint 是：

```
https://dsh-otel-collector.deepseeksvc.com/v1/logs
```

**默认会把沙箱内的 trace 发往官方收集端。** 删包 + `disabled: true` 是硬要求，不是省 28MB 的问题。

### 2. npm 平台过滤只在「一次性安装」时可靠

`npm install --os=linux --cpu=x64` 装出来的确实是干净的。但**事后单独 `npm i <任何包>`，npm 会按构建机平台重算整棵树的 optionalDeps**，把 darwin 版 sharp / sherpa / koffi / ripgrep 一次性全拉回来（+125MB）。

⇒ Dockerfile **必须一次装完**。补装只能 `npm pack` + 手工解包 `--strip-components=1`。

### 3. `--patch` 是运行契约，不是可选优化

被禁用的三个插件原本是懒加载，**不传 patch 也能握手成功**，所以很容易误判「不需要」。但只要配置非 DeepSeek provider 或触发遥测上报，就会崩在缺模块的路径上。

⇒ 启动命令**必须**带 `--patch /opt/dsh/trim-patch.yml`，且 provider 锁定 `deepseek-official`。

## 验证口径

```
initialize → session/new → prompt
```

**`session/new` 这一步不能省。** 只测 `initialize` 会漏掉问题：`session/new` 才真正触发 schema 校验（zod）和 session 持久化（原生 binding）。实测中正是这一步暴露了版本对齐问题。

## 后果

### 好的

- 镜像从「八百多兆」降到压缩 25MB，冷启动和节点磁盘压力都消失
- overrides 是声明式的，可复现，不依赖手工删目录

### 坏的

- **裁剪与 dsh 内部结构耦合**：知道「哪些包 ACP profile 不加载」依赖对 dsh 插件体系的了解。**dsh 升级后必须重跑 `dsh-trim/trim.sh` 并重新验证**，不能直接换版本号
- **能力面不会因裁剪而改善**：`image` / `audio` / `embeddedContext` 仍是 false，这是 dsh 的设计选择
- 删掉多厂商 SDK 后，**切换非 DeepSeek provider 会崩**。这是有意的取舍，要写在运维文档里

## 被否掉的替代方案

| 方案 | 否掉的原因 |
|---|---|
| 只删目录不做 overrides | 只能省 373MB，最大头（zod 副本 322MB）删不掉——它不是一个可以独立删的包，是 46 处嵌套副本 |
| pnpm 安装 + 硬链接去重 | 镜像里 pnpm store 的布局会让 `node_modules` 结构变化，dsh 的插件加载器对路径有假设，风险高于收益 |
| 保留 Web UI profile | 62 个 client-ui 包共 106MB，ACP 模式下完全不加载 |
| 用 `danger-full-access` 简化一切 | 放弃内层防线。VM 隔离是外层，不等于可以关掉 agent 自身的路径约束 |

## 附：怎么复核

```bash
cd dsh-trim
bash trim.sh              # 每步打印体积，可逐项对照
node verify-acp.mjs --patch ./trim-patch.yml
```

`trim.sh` 的每一步都对应上表一行，删错能立刻定位。
