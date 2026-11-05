---
name: astra-run
description: 把耗时任务提交给 Astra Agent 沙箱并等待结果，适用于构建、批量改文件、跑测试这类几分钟起步的操作
version: 1
requires: [astra-cli]
---
# 何时使用

当任务满足**任意一条**时，用 `astra-run` 而不是在本机直接执行：

- 预计超过 30 秒（`go build ./...`、`npm ci`、大批量重命名）；
- 需要隔离环境（不确定的依赖安装、可能写坏文件的脚本）；
- 用户明确说「在沙箱里跑」「别动我本地」；
- 需要留下可回看的事件记录（出了问题要复盘工具调用顺序）。

不要用的场景：读一个文件、算一道题、`git status` 这种秒回的操作——
起沙箱比干活还慢。

# 怎么用

```bash
astra-cli run "指令" --wait
```

- `--wait`：提交后跟随事件流直到终态，把回答实时打到 stdout。
  退出码 `0=completed`、`1=failed/cancelled`、`2=用法错误`。
- `--timeout 10m`：等待上限，默认 5 分钟。**超时不代表任务失败**——
  服务端的 run 还在跑，只是 CLI 不等了，退出码为 1。
- `--session <id>`：接着已有会话干（能继承上下文）。第一次会自动建会话，
  会话 id 打在 stderr 上。
- `--json`：不打印过程，最后输出 run 的完整 JSON（含 `status` / `error` /
  `nextSeq`），给程序判断用。

标准用法：

```bash
astra-cli run "跑 go test ./internal/... 并总结失败原因" --wait --timeout 10m
```

事件流里会看到三类输出：

```
→ bash({"command":"go test ./internal/..."})     工具调用（标题 + 入参摘要）
FAIL	github.com/.../store	0.42s                     回答正文（message_delta 原样输出）
! 需要审批：写入 /etc/hosts                        需要人工批准（approval_req）
```

# 超时了怎么办

`--wait` 超时后**不要重新提交**——同一个任务重跑一遍可能已经在改了同一批文件。
先用 run id 续上：

```bash
astra-cli run logs <run-id>            # 从头发一遍历史，然后继续跟
astra-cli run logs <run-id> --after 88 # 从 seq 88 之后续传（历史太长时用）
```

真要继续跟很久，加 `--timeout 0`（不限时）或把 `--wait` 去掉自己轮询：

```bash
astra-cli run "长任务" --json          # 只提交，拿到 run id
astra-cli run logs <run-id>            # 需要时再跟
```

# 需要审批时

策略是 `ask` 时，写文件、执行白名单外命令会挂起等人：

```
! 需要审批：写入 /etc/hosts
  批准：astra-cli approve <run-id> tc_9f3 --allow
```

先 `astra-cli approvals <run-id>` 看完整列表（含 kind、过期时间），再
`astra-cli approve <run-id> <tool-call-id> --allow|--deny`。
**不要替用户批量 allow**：审批的意义就是让人看一眼。

# 注意

- 一次 `run` 是一条指令。要「先A再B」请写成一条自然语言指令，
  不要并发发多个 run 到同一个会话——同一会话的 run 是串行的，并发提交只会排队。
- 指令要写**可验证的完成条件**（「跑测试并给出失败用例」优于「看看有没有问题」），
  否则 agent 会在沙箱里反复试探直到超时。
- 沙箱里的文件不在本地磁盘上。要看产物用 `astra-files`（`files ls/cat/download`）。
- 失败时先看 `run logs` 的 `error` 事件与最后一个工具调用，再决定重试；
  `failed` 的 run 里 agent 的中间输出仍然是有效信息。
