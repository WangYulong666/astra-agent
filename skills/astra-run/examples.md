# astra-run 示例

## 场景 1：跑测试并总结（正常完成）

```bash
astra-cli run "在仓库根目录跑 go test ./internal/... ，把失败用例和原因列出来" --wait --timeout 8m
```

期望输出：

```
会话：a37c3e69c36d97c0005f97cb
run：43ae52e575acfd68ebc8960c
→ bash({"command":"go test ./internal/...","description":"Run unit tests"})
ok  	github.com/WangYulong666/astra-agent/internal/store	0.42s
--- FAIL: TestReap (0.01s)
    reaper_test.go:88: 期望回收 2 个，实际 1 个
FAIL
共 12 个包：11 通过，1 失败

完成（completed）
```

退出码 `0`。要点：最后一行 `完成（completed）` 打在 stderr，正文在 stdout，
所以 `astra-cli run ... --wait > out.txt` 拿到的是纯回答。

## 场景 2：--wait 超时，用 run logs 续上

```bash
astra-cli run "npm ci && npm run build" --wait --timeout 90s
```

期望输出：

```
会话：9ae0aeaacde84e425176358d
run：7f21c0aa91be3d5c8a1b7722
→ bash({"command":"npm ci"})
added 812 packages in 74s

等待超时（1m30s）。任务仍在服务端跑，用 astra-cli run logs 7f21c0aa91be3d5c8a1b7722 继续跟。
```

退出码 `1`（超时按失败处理，但任务没死）。接着：

```bash
astra-cli run logs 7f21c0aa91be3d5c8a1b7722
```

期望输出（从历史回放，然后继续实时）：

```
→ bash({"command":"npm run build"})
vite v5.4.2 building for production...
✓ 1423 modules transformed.
dist/index.html  0.48 kB

run 结束：completed
```

要点：不要重新 `run "npm ci && npm run build"`——那会把已装好的 node_modules 和
构建产物再折腾一遍。

## 场景 3：需要审批（写白名单外的路径）

```bash
astra-cli run "把构建产物同步到 /srv/www/astra 并 reload nginx" --wait
```

期望输出：

```
run：c81d4f0e22ab77c0b1e9a3d4
! 需要审批：写入 /srv/www/astra
  批准：astra-cli approve <run-id> tc_9f3 --allow
· 等待人工审批
```

此时 run 停在 `waiting_approval`，`--wait` 会一直等到审批超时（服务端默认按拒绝处理）。

先看列表再决定：

```bash
astra-cli approvals c81d4f0e22ab77c0b1e9a3d4
```

```
TOOL-CALL-ID  STATUS   KIND        TITLE                 EXPIRES
------------  -------  ----------  --------------------  ----------------
tc_9f3        pending  edit       写入 /srv/www/astra   2026-11-06 00:41
```

批准：

```bash
astra-cli approve c81d4f0e22ab77c0b1e9a3d4 tc_9f3 --allow
```

```
已提交审批：allow（run c81d4f0e22ab77c0b1e9a3d4，tool tc_9f3）
```

要点：审批是**授权 agent 越界执行**，先看 `TITLE` 与目标路径再点 allow；
不确定就 `--deny`，让 agent 换一条更安全的路径重试。
