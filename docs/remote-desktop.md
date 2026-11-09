# 远程桌面：用手机看 / 控这台电脑

人不在电脑跟前，但电脑上的 agent 卡住了、弹了个框、服务没起来。
这个功能让你用手机浏览器看到电脑屏幕、按几个按钮把机器弄回正轨，
必要时接管鼠标键盘。

> 面向使用者。设计取舍与协议细节见 [`docs/design/remote-desktop.md`](./design/remote-desktop.md)。

---

## 1. 能解决什么

| 场景 | 用哪一层 | 需要什么权限 |
|---|---|---|
| 想知道 agent 现在跑到哪了 | **监看**（实时画面） | 屏幕录制 |
| 弹了个对话框，需要点「允许」 | **键鼠接管** | 屏幕录制 + 辅助功能 |
| 服务没起，要打开终端敲一条命令 | **键鼠接管** | 同上 |
| 只想锁屏 / 打开一个网址 / 存一张截图 | **快捷操作** | 无 |
| 想让 agent 去干这件事 | 会话里直接发指令 | 无（走已有的对话与审批） |

三层能力**互相独立**：没有辅助功能权限时，画面和快捷操作照常可用，
只是接管按钮会告你「缺权限」——这不是坏了，是设计内的降级（见 §7）。

**刻意不做**（不是没做完，是权衡后不做）：

- 剪贴板双向同步（键盘输入可能含密码，同步剪贴板等于把它送去手机）
- 文件传输（走已有的资料库 / 文件接口，别再造一条通道）
- 音频、录屏、多显示器切换
- 公网穿透：本服务不监听公网，必须由你自己用反向代理 + TLS 暴露

## 2. 电脑端要开哪些权限

macOS（系统设置 → 隐私与安全）：

| 权限 | 路径 | 勾选什么 | 缺了会怎样 |
|---|---|---|---|
| 屏幕录制 | 隐私与安全 → **屏幕录制** | 运行 `astra-server` 的程序（Terminal / iTerm / 打包后的 astra-agent） | 手机端一直黑屏；`screencapture` 产出的图只有几 KB |
| 辅助功能 | 隐私与安全 → **辅助功能** | 同上 | 能看不能控，接管时报 `permission_denied` |
| 本机网络（可选） | 隐私与安全 → 本地网络 | 同上 | 只在用 mDNS/局域网发现时才相关，反代访问不需要 |

命令行方式打开设置面板：

```bash
open "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
open "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
```

授权后**要重启一次 astra-server**（macOS 的 TCC 权限在进程启动时生效；
能力探测本身只缓存 30 秒，但 TCC 的变更要新进程才拿得到）。

Linux：

| 能力 | 依赖 | 说明 |
|---|---|---|
| 抓屏 | `gnome-screenshot`（Wayland 优先）/ `scrot` / `import`（ImageMagick） | 三者任一即可，按顺序探测 |
| 注入 | `xdotool` | Wayland 下通常不可用，会明确报错，不会静默失败 |

```bash
sudo apt install gnome-screenshot   # Wayland
sudo apt install scrot xdotool      # X11
```

**一条命令看全**（推荐先跑这个，它会把缺什么、去哪儿改直接打出来）：

```bash
make remote-check
# 或（要有 token）
ASTRA_TOKEN=<admin token> make remote-check
```

```
平台          Darwin arm64
抓屏·监看     OK      screencapture · 1920×1080 · 39565 字节
注入·键鼠接管 不可用  辅助功能未授权（System Events 报 -10004）：系统设置 → 隐私与安全 → 辅助功能，勾选终端 / astra-server
控制面接口    OK      GET http://127.0.0.1:7788/v1/remote/capabilities → 200
可用模式      只看（view）+ 快捷操作（control 会被降级为只读）
```

等价的接口级检查：`admin-cli remote capabilities`（`--json` 给脚本用，
`--strict` 让任意一层不可用都返回 1，适合放进部署自检）。

## 3. 三步配对

**第 1 步：电脑上生成配对码**

```bash
admin-cli remote pair --device "书房的 Mac"
```

```
配对码    284913
剩余      118 秒（2026-11-10 10:02 UTC 到期）
设备名    书房的 Mac
payload   astra://remote/pair?code=284913&server=http%3A%2F%2F127.0.0.1%3A7788
深链      astra://remote/pair?code=284913&server=http://127.0.0.1:7788
```

控制面的「远程控制」页面也能出码，那里会直接把 `payload` 画成二维码。

**第 2 步：手机上绑定**

手机打开控制面 → 远程控制 → 添加电脑 → 输入 6 位码（或扫码）。
绑定成功后这台电脑出现在设备列表里：

```bash
admin-cli remote devices
```

```
设备ID  名称       平台    状态    最后活跃 (UTC)
------  ---------  ------  ------  --------------------
dev-1   书房的 Mac  darwin  在线    2026-11-10 10:00 UTC
```

**第 3 步：开一路会话**

手机端点设备即可。命令行等价操作：

```bash
admin-cli remote start --device dev-1 --mode view      # 只看
admin-cli remote start --device dev-1 --mode control   # 申请接管
```

`--mode control` 建出来的会话是 `pending_approval`，**必须**在电脑上确认一次：

- 控制面页面点「允许」，或
- `admin-cli remote approve <sessionID>`

批准后状态转 `active`，手机上的点击才开始真的落到这台电脑上。

> 配对码规则：6 位数字、**120 秒**有效、**只能用一次**、连续错 5 次作废。
> 它证明的是「此刻有人在这台电脑跟前」，不是权限凭证——身份始终来自登录态。

脚本/手机侧绑定（把本机登记成设备）用：

```bash
admin-cli remote claim 284913 --device "我的 iPhone"
```

## 4. 手机上的手势

画面区域支持以下手势（一对一映射到真实鼠标/键盘事件）：

| 手势 | 等效操作 |
|---|---|
| 单指轻点 | 左键单击 |
| 单指长按后拖动 | 左键按下拖动（选中、拖文件） |
| 双指轻点 | 右键 |
| 双指上下滑动 | 滚轮 |
| 双指捏合 / 张开 | 缩放画面（只改本地显示，不改电脑分辨率） |
| 长按画面 → 键盘按钮 | 弹出虚拟键盘，可发文字与 Enter / Backspace / Tab / Esc / 方向键 |

底部工具条：画质档位、全屏、断开。

坐标是**归一化**的（相对当前画面），所以切画质、横竖屏都不会让点击错位。

## 5. 画质档位与省流量

单帧走 JPEG（不是视频编码），档位调的是三件事：宽度、JPEG 质量、帧率。

| 档位 | 大致参数 | 适合 |
|---|---|---|
| 省流 | 宽 ≈960 · 4G 下用 | 只想确认「跑到哪了」 |
| 标准 | 宽 ≈1280 · q≈60 · 约 8fps | 默认 |
| 清晰 | 宽 ≈1600 · q≈75 · 约 10fps | 要看清文字、要操作 |

- 服务端策略封顶：默认 **宽度 ≤1600、帧率 ≤10、质量 ≤95**，档位只能在范围内调。
- 1280 宽 q60 单帧约 80–150KB，8fps 的理论上限约 1MB/s。
- **静止画面不发帧**：服务端把画面缩到灰度小图做对比，没变化就跳过发送，
  另外每 2 秒补一帧心跳帧防「以为断了」。所以盯着一段不动的代码时流量接近 0。
- 手机端掉帧策略是「丢旧帧」，不会越积越卡。

## 6. 快捷操作（不需要任何权限）

优先级最高的按钮：它们**不依赖**辅助功能，所以就算只能看，也能用。

```bash
admin-cli remote action <sessionID> open-app Finder
admin-cli remote action <sessionID> open-url https://example.com
admin-cli remote action <sessionID> screenshot     # 存到 <dataDir>/remote/shots
admin-cli remote action <sessionID> lock           # 锁屏 / 立即熄屏
```

界面上的「快捷操作」面板与这几条命令等价。`screenshot` 返回的是**电脑上的路径**，
不是把图片传回手机——要看图就直接看画面，要留档则去那个目录取。

## 7. 为什么会出现「只读模式」

接管鼠标键盘在 macOS 上需要**辅助功能**权限，而这是系统级授权（TCC），
服务进程没被勾选时，`osascript` 会报 `权限违例 (-10004)`。

于是产品按能力分层：

```
有屏幕录制 → 能看
有屏幕录制 + 有辅助功能 → 能看 + 能控
两者都没有 → 只剩快捷操作（仍然能救场：开网址、锁屏、截屏）
```

- 只读不是 bug，是**明确告诉你缺什么**：接管按钮会显示原因与修复路径，
  而不是静默失败让你以为「手机坏了」。
- 修好权限后**重启 astra-server**，再跑一次 `admin-cli remote capabilities` 确认。
- 不要用 `sudo` 或关掉 SIP 去绕过 TCC：那等于把这个安全边界整个拿掉。

## 8. 安全边界

| 面 | 做法 |
|---|---|
| 配对码 | 6 位、120 秒、一次性、错 5 次作废；不进审计日志（记了就等于公开） |
| 设备 | 绑定到你的账号；列表只返回你自己的；吊销**立即**生效并断开该设备的活动会话 |
| 接管 | `control` 默认待批准，必须在电脑上确认一次；`view` 可配置成免批准 |
| 急停 | 手机断开 / 电脑面板断开 / `admin-cli remote end <id>` / 吊销设备，任意一方都能停 |
| 全局急停 | `ASTRA_REMOTE_DISABLED=1`：所有 `/v1/remote/*` 直接 503，不再受理 |
| 输入限速 | 500ms 内超过 200 个事件直接丢弃并记一次审计，防「被脚本疯狂点击」 |
| **不传** | 剪贴板、文件、音频一律不过这条通道 |
| 审计 | 记 `remote.pair` / `remote.revoke` / `remote.session.start` / `remote.session.end` / `remote.control.grant` / `remote.action`；**不记**配对码、键盘内容、URL 的 query |
| 归属 | 服务端只认登录身份，跨租户一律 404（不区分「不存在」与「无权」，避免探测） |
| 传输 | 画面与控制消息走 WebSocket，必须由反向代理 + TLS 暴露；`ASTRA_REMOTE_ORIGINS` 控制允许的来源 |

```bash
# 全局急停（重启服务后生效；运行中也可以只断单路）
ASTRA_REMOTE_DISABLED=1

# 手机经反代访问时，把反代域名填进来，否则 WS 升级会被 403
ASTRA_REMOTE_ORIGINS=https://astra.example.com
```

## 9. 排障

先跑 `make remote-check`（电脑端），再看下表。

| 现象 | 最可能的原因 | 怎么确认 / 怎么修 |
|---|---|---|
| 手机上是**黑屏**，一直没画面 | 显示器休眠；或没授予「屏幕录制」 | `make remote-check` 里「抓屏」一行：字节数很小（<8KB）就是全黑。唤醒屏幕，去 系统设置 → 隐私与安全 → 屏幕录制 勾选后**重启服务** |
| **能看不能控**，点击没反应 | 缺「辅助功能」权限 | 同上「注入」一行；`admin-cli remote capabilities` 的 `input.reason` 写着 `-10004`。勾选后重启服务 |
| **连不上**：页面打开就是断线 / WS 403 | 反向代理没配 WebSocket 升级；或来源不在白名单 | 反代要转发 `Upgrade` / `Connection` 头（Nginx 用 `proxy_set_header Upgrade $http_upgrade; proxy_http_version 1.1;`）。跨域访问要设 `ASTRA_REMOTE_ORIGINS=https://你的域名` |
| **帧率低**（1–5fps 且画面糊） | 抓屏是「起一次系统命令抓一张图」，单帧本身约 200ms | 这是设计边界（不依赖 ffmpeg/WebRTC）。降一档画质、或缩小窗口；手机端掉帧是「丢旧帧」，不会越用越卡 |
| **会话一直 pending**，手机点不动 | 没在电脑上确认接管 | 控制面「远程控制」面板点「允许」，或 `admin-cli remote approve <sessionID>` |
| 所有接口 **503** | 全局急停 / 能力未装配 | `ASTRA_REMOTE_DISABLED` 是否为 1；看服务启动日志里「远程桌面已启用」是否出现 |
| **404** `设备或会话不存在` | id 写错，或那台设备属于别的账号 | `admin-cli remote devices` / `remote sessions` 重新取 id；跨账号看不到是设计如此 |

常用对照命令：

```bash
admin-cli remote capabilities          # 三层能力 + 缺什么 + 怎么修
admin-cli remote devices               # 设备是否在线 / 是否被吊销
admin-cli remote sessions --limit 20   # 有没有会话在跑、帧率与掉帧
admin-cli remote end <sessionID>       # 急停
```
