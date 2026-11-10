# 远程桌面 v2：补「外网可达」与「操控级帧率」

## 0. 为什么有 v2

v1（8eb9041）把功能做全了（配对/审批/急停/四层降级），但对着 WorkBuddy 一比，
有两条硬伤：

| 硬伤 | v1 | WorkBuddy |
|---|---|---|
| 外网可达 | 要求用户自备反向代理 + TLS + 域名 + 配 `ASTRA_REMOTE_ORIGINS` | 电脑端主动出站，云中继，扫码即用，不碰路由器 |
| 帧率 | `screencapture` 子进程 110–140ms/帧，实测 ~5fps | WebRTC，25fps+ |

「让用户自己配反向代理」等于把最难的一步丢给他——这是产品级问题，不是调参。

## 1. 本机实测（决定方案的两条数据）

2026-11-10 实测（Mac mini M4 / macOS 26.2）：

```
screencapture -x -t png   110–140ms/帧  → 上限 ~7fps
cgo CGWindowListCreateImage（dlsym）  28–31ms/帧
  + box 下采样 1280 宽 + jpeg q60     16ms
  合计 44–47ms/帧 → 上限 21–23fps        ← 快 4–5 倍
```

两个必须知道的坑：
1. `CGWindowListCreateImage` 在 macOS 15 SDK 里被标记 **obsoleted**（编译期不可用），
   但符号仍在 CoreGraphics 中。用 `dlsym` 运行时取，符号真被移除时返回空 → 自动回退
   `screencapture`。
2. **屏幕休眠时**：`CGWindowListCreateImage` 一次要 **940ms**，`screencapture` 返回
   全黑 PNG。v2 必须显式检测（单帧 >500ms 或全黑）并进入退避，否则会把 CPU 打满
   还什么也发不出去。

## 2. 两条管道

### A. 反向隧道（relay）——解决「能不能连上」

```
手机 ──HTTPS/WSS──▶ relay（公网，astra-relay）◀──出站长连接── 电脑（astra-server）
                     /t/{device}/...                 smux 多路复用
```

- 电脑端**主动出站**连 relay（NAT 放行），不需要公网 IP、不开端口、不改路由器。
- relay 用 `xtaci/smux`（MIT）在一条 WSS 上多路复用；每个 HTTP 请求一条逻辑流。
- relay 的转发用 `httputil.ReverseProxy` + 自定义 `DialContext`，**WebSocket 升级也走同一条路**
  （ReverseProxy 原生支持 101 升级后的双向 copy），不用手写隧道。
- relay **不做业务解析、不校验平台 JWT**——鉴权仍在 origin。relay 只做：
  设备注册鉴权（HMAC）、未知设备拒绝、并发流上限、空闲超时。
- 设备 id 必须是**不可猜**的（默认取 hostname 只是便于识别，正式用应在配对时下发随机 id）。

### B. WebRTC（Pion）——解决「点得准不准、跟不跟手」

诚实的前提：**我们没有视频编码器**。纯 Go 没有可用的 H.264/VP8 编码器，
VideoToolbox 硬件编码是另一个 cgo 项目（风险高，本轮不做）。所以：

- DataChannel 上发的仍是**单帧 JPEG**（分片传输）。
- WebRTC 在这里买到的是：**P2P 直连**（局域网/打洞成功时少一跳）、
  **SCTP 拥塞控制**、**ICE 自动选路**；**不是带宽优化**——带宽仍由 JPEG 质量/分辨率决定。
- 打洞失败（对称 NAT、无 TURN）→ 自动回退 v1 的 WS + JPEG。**回退路径永远可用**，
  这条是硬约束：任何 WebRTC 故障都不能让会话失败。
- 非 trickle：客户端聚合完候选发 offer，服务端聚合完候选回 answer。省一套 ICE 信令状态机。

要真正省带宽/上 30fps 稳定，下一步是 VideoToolbox H.264（macOS）/ VAAPI（Linux），
那是独立一轮，不在 v2 范围。

## 3. 文件边界

| 任务 | 独占文件 |
|---|---|
| W1 relay | `internal/relay/**`、`cmd/astra-relay/**`、`docs/relay.md`、`Makefile` |
| W2 媒体 | `internal/remotedesk/capture*.go`、`webrtc*.go`、`stream.go`、`internal/api/remote_ws.go`、对应测试 |
| W3 前端/文档 | `web/src/**`、`docs/remote-desktop.md`、`.env.example` |

主控独占：`internal/remotedesk/remotedesk.go`、`internal/api/api.go`、`openapi*`、`internal/app/*`。

## 4. 验收

- relay：本机起 astra-server + astra-relay + 隧道，经 `http://relay/t/{dev}/v1/meta` 拿到
  真实响应；经 relay 的 WS 能收到真帧（这是唯一能证明"外网可用"的验收）。
- 媒体：cgo 后端真机帧率 ≥15fps（1280 宽）；WebRTC 在局域网建立 DataChannel 并收到帧；
  WebRTC 失败时自动回退且画面不断。
- 前端：能在「直连 / 中继」之间显示当前链路；WebRTC 与 WS 两种来源都能渲染。
