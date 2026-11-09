# 远程桌面（手机看/控电脑）

## 0. 为什么做、做到哪一档

用户场景：电脑上 agent 跑任务卡住了、或者电脑本身出问题（服务没起、窗口弹框、要手动点一下），
人不在跟前。手机打开控制面就能**看到屏幕**、**接管鼠标键盘**、或**按快捷按钮**把机器弄回正轨。

对标 WorkBuddy 的远程控制（2026-10 上线）：同账号 + 扫码/配对码绑定 → 手机看画面 →
手势接管（单指点击=左键、双指=右键、双指滑动=滚轮、长按拖动、捏合缩放）→
画质档位 + 虚拟键盘 + 快捷按钮（显示桌面/切窗口）→ 随时断开。
它还有 IM 通道（微信/企微/QQ/飞书）下发指令，我们这轮不做（通道适配是另一条线）。

### 本机实测逼出来的分层（关键，别设计成"全有或全无"）

2026-11-10 在本机（macOS 26.2 / M4）实测：

| 能力 | 实测结果 |
|---|---|
| `screencapture -x -t png` | ✅ 成功，1920×1080、39KB，画面真实 |
| 辅助功能（注入鼠标键盘） | ❌ `System Events` 报 `权限违例 (-10004)`，TCC 不给 |
| ffmpeg / cliclick / xdotool | ❌ 都没装 |

所以产品必须按能力分层，**每一层单独可用、缺权限时明确告诉用户缺什么**：

| 层 | 能力 | 依赖 | 本机 |
|---|---|---|---|
| L1 监看 | 实时画面（JPEG 帧） | 屏幕录制权限 | ✅ 可用 |
| L2 快捷操作 | 打开 App / 打开网址 / 截屏存盘 / 锁屏 | 无特殊权限（`open`、`pmset`） | ✅ 可用 |
| L3 键鼠接管 | 点击/拖拽/滚轮/键盘 | 辅助功能权限 + `osascript` 或 `cliclick` | ❌ 会明确报 `permission_denied` |
| L4 指令通道 | 下发一句话让 agent 干活 | 已有 run/审批体系 | ✅ 复用，不新建通道 |

L4 **刻意不新建 exec 接口**：平台已经有会话/run/审批，再开一个"远程执行 shell"就是给自己加一个 RCE 面。

### 明确不做（本轮）

- 剪贴板双向同步（隐私风险高，收益低）
- 文件传输（走已有的资料库/文件接口）
- 音频、多显示器选择、录屏存档
- WebRTC（需要 STUN/TURN + 信令 + UDP 端口，自部署场景运维负担大；
  单帧 JPEG over WS 在局域网/4G 上都够用，且只需 `<canvas>`）
- 公网穿透：**必须**由部署方用反向代理 + TLS 暴露，服务本身不监听公网 UDP/明文

## 1. 传输设计

```
手机浏览器                        astra-server
  |  POST /v1/remote/sessions       （建会话，拿 sessionId）
  |  GET  /v1/remote/sessions/{id}/stream?ticket=xxx   （WS 升级）
  |<---- binary: 单帧 JPEG --------|  抓屏泵（共享，1 个/会话）
  |---- text: {"type":"input"} --->|  注入后端（L3）
  |---- text: {"type":"quality"} -->|  调 fps/宽度/质量
  |<---- text: {"type":"hello","stats":...}
```

为什么不用 MSE/H.264：需要封装容器 + 浏览器兼容差异；JPEG 帧直接 `createImageBitmap` 画 canvas，
**10 行代码**，掉帧策略也好写（丢旧帧，绝不排队）。带宽：1280 宽 q=60 单帧约 80–150KB，
8fps 约 1MB/s，局域网/4G 可接受；静止画面靠"帧内容对比"直接不发（见 §2）。

WebSocket 鉴权复用现成的一次性票据：`POST /v1/auth/sse-ticket` 换 60 秒 `scope=sse` 票据，
WS URL 带 `?ticket=`。浏览器 WS 无法自定义 header，这是仓库里已有的解法，不另造。

## 2. 抓屏（零外部依赖）

macOS：`screencapture -x -t png <tmp>` → Go `image/png` 解码 → 自写 box 下采样到目标宽度 →
`image/jpeg` 编码（质量可调）。全程 stdlib，不依赖 ffmpeg。

Linux：`gnome-screenshot -f` / `scrot` / `import`（ImageMagick）任一存在即可，同样走 Go 重编码；
Wayland 下 `gnome-screenshot` 可用而 `import` 通常不行，探测时按平台给 hint。

其他平台：`Capability.Available=false`，`Reason` 写清楚，不假装支持。

**静止画面不发帧**：下采样到 48×27 灰度求平均绝对差，低于阈值就跳过（用户在看代码不动时省 90% 流量）。
强制定期发一帧心跳帧（每 2 秒），防手机端以为断了。

## 3. 安全

| 面 | 做法 |
|---|---|
| 身份 | 不新增 token 类型。手机用正常登录态；配对码只证明"人在电脑跟前" |
| 配对 | `POST /v1/remote/pairing` 生成 6 位码 + 深链，TTL 120 秒、一次性、失败 5 次作废 |
| 设备 | 绑定 tenant/user；列表只返回自己的；吊销立即生效（会话同时关闭） |
| 会话 | `view` 可直接看（可配），`control` 默认 **pending_approval**，要桌面端点批准 |
| 归属 | 每次取会话都校验 `TenantID`，跨租户一律 404（不区分"不存在"与"无权"，避免探测） |
| 输入 | 500ms 内 >200 事件即丢弃并记一次审计；`Policy.AllowInput=false` 时直接拒 |
| 审计 | `remote.pair` / `remote.revoke` / `remote.session.start` / `remote.session.end` / `remote.control.grant` / `remote.action` |
| 急停 | `DELETE /v1/remote/sessions/{id}` 任一方可断；`ASTRA_REMOTE_DISABLED=1` 全局关 |

## 4. 文件边界（并行任务契约）

主控独占（冻结，agent 不许改）：

- `internal/remotedesk/remotedesk.go` —— 契约
- `internal/api/api.go`、`internal/api/openapi.yaml`、`internal/api/openapi_test.go`
- `internal/app/*`

| 任务 | 独占文件 |
|---|---|
| R1 核心 | `internal/remotedesk/manager.go`、`store.go`、`pairing.go`、`policy.go` + 测试 |
| R2 抓屏/注入/流 | `internal/remotedesk/capture*.go`、`input*.go`、`stream.go`、`fake.go`、`probe*.go` + 测试 |
| R3 API | `internal/api/remote_admin.go`、`remote_ws.go`、`remote_test.go`（**删掉主控放的 `remote_stub.go`**） |
| R4 前端 | `web/src/**` |
| R5 CLI/文档 | `cmd/admin-cli/cmd_remote.go`、`internal/client/remote.go`、`docs/remote-desktop.md`、`Makefile`、`.env.example` |

跨任务只通过 `remotedesk.go` 的类型与接口；实现内部结构各自定，不互相 import。

## 5. 验收

- 单测 + `-race` 全绿；`gofmt`/`vet` 干净
- 真机：`GET /v1/remote/capabilities` 报 `capture.available=true`（本机实测）
- 真机：能抓到真画面帧（非黑屏，尺寸正确）
- 真机：L3 在本机应报 `permission_denied` 且前端明确提示，而不是静默失败
- 前端 393px 不溢出，手势映射齐全，有截图
- **如实标注**：L3 在本机无法端到端验证（TCC 不给），只能验证错误路径
