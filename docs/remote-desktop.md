# 远程桌面：用手机看 / 控这台电脑

人不在电脑跟前，但电脑上的 agent 卡住了、弹了个框、服务没起来。
这个功能让你用手机浏览器看到电脑屏幕、按几个按钮把机器弄回正轨，
必要时接管鼠标键盘。

> 面向使用者。设计取舍见 [`docs/design/remote-desktop.md`](./design/remote-desktop.md)
> 与 [`docs/design/remote-desktop-v2.md`](./design/remote-desktop-v2.md)；
> relay 的部署与运维见 [`docs/relay.md`](./relay.md)。

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
只是接管按钮会告你「缺权限」——这不是坏了，是设计内的降级（见 §10）。

**手机在外面（4G/公司网）能不能连上**，取决于有没有部署 relay：见 §4。
不部署 relay 也能用，但只限同一局域网。

**刻意不做**（不是没做完，是权衡后不做）：

- 剪贴板双向同步（键盘输入可能含密码，同步剪贴板等于把它送去手机）
- 文件传输（走已有的资料库 / 文件接口，别再造一条通道）
- 音频、录屏、多显示器切换
- 视频编码（H.264/VP8）：DataChannel 上跑的仍是单帧 JPEG，见 §5

## 2. 电脑端要开哪些权限

macOS（系统设置 → 隐私与安全）：

| 权限 | 路径 | 勾选什么 | 缺了会怎样 |
|---|---|---|---|
| 屏幕录制 | 隐私与安全 → **屏幕录制** | 运行 `astra-server` 的程序（Terminal / iTerm / 打包后的 astra-agent） | 手机端一直黑屏；`screencapture` 产出的图只有几 KB |
| 辅助功能 | 隐私与安全 → **辅助功能** | 同上 | 能看不能控，接管时报 `permission_denied` |
| 本机网络（可选） | 隐私与安全 → 本地网络 | 同上 | 只在用 mDNS/局域网发现时才相关，经 relay 访问不需要 |

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

控制面的「远程桌面」页面也能出码（大号 6 位数 + 一键复制深链，不画二维码）。

**第 2 步：手机上绑定**

手机打开控制面 → 远程桌面 → 添加设备 → 输入 6 位码。绑定成功后这台电脑出现在设备列表里：

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

## 4. 外网怎么连（手机在 4G/公司网）

默认情况下控制面只监听本机，手机必须能**直接访问到电脑的地址**——也就是同一个
Wi-Fi、或者你自己做的端口映射。出门在外想连上，需要部署一台公网中继 `astra-relay`。

三条链路的关系：

```
手机 ──HTTPS/WSS──▶ relay（公网机器）◀──出站长连接── 电脑（astra-server）
                     /t/{device}/...              （不需要公网 IP、不开端口）
```

**电脑是主动往外连的**，所以：

- **不需要公网 IP**，不用在光猫/路由器上做任何端口映射，不用改 NAT、不用 DDNS；
- 家里那台机器不对外暴露任何端口——relay 只认「已经用 token 注册上来的」设备；
- 手机访问的是 relay 的公网地址，走标准 443。

### 4.1 部署 relay（一次，五分钟）

完整步骤（systemd、TLS 两种终结方式、反代 WSS 的三个坑）见
[`docs/relay.md`](./relay.md)。最小版本：

```bash
# 1. 公网机器上编译并启动（方式 A：relay 自己终结 TLS）
make build                      # 产物 bin/astra-relay
bin/astra-relay \
  -addr :9443 \
  -token "$(openssl rand -hex 32)" \
  -tls-cert /etc/letsencrypt/live/relay.example.com/fullchain.pem \
  -tls-key  /etc/letsencrypt/live/relay.example.com/privkey.pem \
  -public-url https://relay.example.com
```

### 4.2 电脑端接入

```bash
# 必填：relay 地址（wss://）
export ASTRA_RELAY_URL=wss://relay.example.com
# 必填：与 relay 的 -token 完全一致
export ASTRA_RELAY_TOKEN=<那串长随机数>
# 选填：设备 id，决定手机访问路径 /t/<device>/，不填取 hostname
export ASTRA_RELAY_DEVICE=my-mac-mini

astra-server
```

启动日志出现下面这行就是通了：

```
外网中继已启动：wss://relay.example.com （设备 my-mac-mini）
```

设备 id 会出现在公网 URL 里，等于一个「账号名」。别用 `mac-mini` 这种可猜的名字：
虽然没 token 猜中了也只能拿到 503，但会被人反复试探。用 `my-mac-mini-a1b2c3` 这种。

### 4.3 手机访问

```
https://relay.example.com/t/my-mac-mini/
```

后面就是正常的控制面：登录、设备列表、远程桌面。路径前缀由 relay 自动剥掉，
不用在手机上做任何配置。

控制面「远程桌面」页顶部有一条**链路状态**：

| 显示 | 含义 |
|---|---|
| `直连` + 绿点 | 手机直接连到电脑（同一局域网 / 你自己做的反代） |
| `经中继` + 绿点 | 手机经 relay 访问，隧道在连 |
| `中继断开` + 黄点 | 配了 relay 但没连上，见 §12 第 7 条 |
| `未配置外网中继：手机只能在同一局域网访问` | 没配 `ASTRA_RELAY_URL`，出门就连不上 |

### 4.4 没部署 relay 会怎样

什么都不坏：手机在同一 Wi-Fi 下、或者你用 nginx 之类自己做了反代，一切照旧。
只是**手机切到 4G 就连不上**。这时页面顶部那条黄字说的就是这件事。

## 5. 画面从哪来：WebRTC 优先，失败自动回退

v2 有两条画面通道，浏览器会**先试 WebRTC，不行就静默退回 WS**：

| | WebRTC（DataChannel） | WS + JPEG |
|---|---|---|
| 路径 | 打洞成功时**点对点**，不经 relay | 经 relay / 反代回到电脑 |
| 延迟 | 少一跳，拥塞控制更跟手 | 多一跳 |
| 建立条件 | 双方能直连（局域网最容易，跨运营商 NAT 常失败） | 只要 HTTP 通就能用 |
| 失败时 | **自动回退**，画面不断、不弹错 | —— |

顶部标签会写清楚现在是哪条：`WebRTC 直连` + `P2P`，或者 `中继(WS)` + `relay`。
回退不是故障，把鼠标悬停在标签上能看到原因（例如「等 rtc-answer 超时」）。

### 为什么开了 WebRTC 也不省流量

这一点必须说清楚，否则很容易误会：

- **DataChannel 里跑的仍是单帧 JPEG**，一帧一张图、分片发送，和 WS 那条路
  **编码完全一样**。纯 Go 没有可用的 H.264/VP8 编码器，这一轮没做视频编码。
- WebRTC 买到的是**直连**（少一跳）和**SCTP 拥塞控制**（网络抖动时更跟手），
  **不是带宽优化**。同样的画质档位，两条路的字节数在一个量级。
- 想省流量，只能降档 —— 用「省流量」档（见 §7），或者干脆只看文字/日志。

真要「又清楚又省带宽」，需要 VideoToolbox/VAAPI 硬件编码，那是另一轮的事。

## 6. 手机上的手势、指针与快捷键

### 6.1 手势

画面区域支持以下手势（一对一映射到真实鼠标/键盘事件）：

| 手势 | 等效操作 |
|---|---|
| 单指轻点 | 左键单击 |
| 单指双击 | 双击（进目录、开文件） |
| 单指长按后拖动 | 左键按下拖动（选中、拖文件） |
| 双指轻点 | 右键 |
| 双指上下滑动 | 滚轮 |
| 双指捏合 / 张开 | 缩放画面（只改本地显示，不改电脑分辨率） |

坐标是**归一化**的（相对当前画面），所以切画质、横竖屏、缩放都不会让点击错位。

### 6.2 虚拟指针（点不准的时候用它）

手指压在目标上，目标就被自己的手指挡住了——点小按钮、点菜单项基本靠运气。
画面上那个**十字圆圈**就是虚拟指针：

1. 按住十字，把它拖到目标上（拖动过程会实时移动电脑上的光标）；
2. 松手，位置就定住了；
3. 点工具条上的 **「点击」**，就在那个位置点一下左键。

指针位置按比例记着：换画质、转横屏、窗口大小变了都不会跑偏。轻点画面时，
指针会跟到刚才点的位置，不用再把十字拖回来。不想看它就用工具条的「指针」开关关掉。

### 6.3 快捷键条

工具里那一排横向可滚动的按钮：`Enter` `Esc` `Tab` `⌫` `↑` `↓` `←` `→`
`⌘C` `⌘V` `⌘Z` `⌘Tab`。点一下就是按一下，比弹系统键盘快。

最左边的 **`⌘`** 是个开关：点亮之后，再点那个键就会**带上 cmd**
（想发 ⌘←、⌘Enter 这类组合时用）。`⌘C/⌘V/⌘Z/⌘Tab` 自带 cmd，不受它影响。

> ⌘C / ⌘V 只是「按下复制/粘贴键」，剪贴板**不过这条通道**：
> 粘出来的是电脑自己剪贴板里的东西，不是手机上的。

要发一整段中文/长文本，用下面的「虚拟键盘」输入框，点「发送」。

底部工具条：画质档位、缩放、快捷操作、立即断开。

## 7. 画质档位与省流量

单帧走 JPEG（不是视频编码），档位调的是三件事：宽度、JPEG 质量、帧率。

| 档位 | 参数 | 单帧大小（约） | 适合 |
|---|---|---|---|
| **省流量** | 宽 480 · 4fps · q40 | 10–20KB，一分钟 3–4MB | 4G 按流量计费，只想确认「跑到哪了」 |
| 低 | 宽 640 · 6fps · q45 | 20–40KB | 移动网络下的日常 |
| 中（默认） | 宽 960 · 8fps · q60 | 50–100KB | 看代码能认清字 |
| 高 | 宽 1600 · 12fps · q75 | 100–200KB | 局域网/家用 Wi-Fi，要看清细节 |

- 服务端还会再夹一次（`clampInt`）：宽度 160–3840、帧率 ≤ 策略上限（默认 **10fps**）、
  质量 ≤100。所以「高」档写的 12fps 在默认策略下实际跑 10fps——档位只会往下调，
  不会突破服务端上限。上限值与「本机能力」卡片上的 `上限 N fps` 是同一个数。
- **静止画面不发帧**：服务端把画面缩到灰度小图做对比，没变化就跳过发送，
  另外每 2 秒补一帧心跳帧防「以为断了」。盯着一段不动的代码时流量接近 0。
- 手机端掉帧策略是「丢旧帧」，不会越积越卡；状态条上的「掉帧」是累计比例。
- 不管走 WebRTC 还是 WS，**档位都生效**（两条路编码同一套）。

## 8. 休眠会黑屏

合盖、熄屏、显示器休眠之后，**抓不到画面**：

- macOS 的 `screencapture` 在屏幕休眠时返回一张全黑 PNG；
- 更快的抓屏后端（`CGWindowListCreateImage`）在休眠时一次要 **940ms**，
  也就是帧率掉到 1fps 左右，看起来像卡死。

控制面目前**没有**接管电源管理（不会自己跑 `caffeinate`），所以连接期间
状态条会直说一句「**电脑休眠会黑屏**」——这不是没做，是照实告知。

要保持长时间连接时屏幕亮着，二选一：

```bash
# 临时：连接期间在电脑上跑（Ctrl-C 结束）
caffeinate -d -i -t 7200      # 2 小时内阻止显示器休眠

# 长期：系统设置 → 锁定屏幕 → 「显示器关闭时…」改成「永不」
# 或者 系统设置 → 电池 → 「电池供电时防止自动休眠」（笔记本）
```

合盖休眠属于硬件级动作，`caffeinate` 也拦不住（除非外接显示器/电源）。真要常开，
用「不进入睡眠」的电源方案，并把机器放在通风的地方。

## 9. 快捷操作（不需要任何权限）

优先级最高的按钮：它们**不依赖**辅助功能，所以就算只能看，也能用。

```bash
admin-cli remote action <sessionID> open-app Finder
admin-cli remote action <sessionID> open-url https://example.com
admin-cli remote action <sessionID> screenshot     # 存到 <dataDir>/remote/shots
admin-cli remote action <sessionID> lock           # 锁屏 / 立即熄屏
```

界面上的「快捷操作」面板与这几条命令等价。`screenshot` 返回的是**电脑上的路径**，
不是把图片传回手机——要看图就直接看画面，要留档则去那个目录取。

## 10. 为什么会出现「只读模式」

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

## 11. 安全边界

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
| 传输（直连） | 画面与控制消息走 WebSocket，反代场景必须上 TLS；`ASTRA_REMOTE_ORIGINS` 控制允许的来源 |

### relay 这一层：它能看到明文

必须直说，因为这是选型时最容易忽略的一条：

- **TLS 终结在 relay**（除非你让 relay 只回源明文、前面再挂一层自己的 TLS）。
  所以 relay 是一段**明文可见的管道**：如果 origin 那侧跑的是明文 HTTP，
  那么 relay 上（以及任何能读它内存/网卡的人）**能看到完整的请求与响应体，
  包括 Bearer token 和远程桌面画面**。
- relay **不校验平台 JWT**，也不知道谁有权控制哪台电脑：鉴权、审批、急停全在
  origin 执行。它只做设备注册（HMAC token）、未知设备拒绝、并发流上限、空闲超时。
- 因此：**relay 必须是你自己信任的机器**。用别人的中继 = 把画面交给别人。
- 想再收紧一层：在 origin 上套 TLS（`https://127.0.0.1:8443` 作为上游），
  隧道里走的就是密文，relay 只看到密文。局域网内用 WebRTC 直连时，
  画面也不经过 relay（DTLS 加密的点对点）。

```bash
# 全局急停（重启服务后生效；运行中也可以只断单路）
ASTRA_REMOTE_DISABLED=1

# 直连/反代场景：把反代域名填进来，否则 WS 升级会被 403
ASTRA_REMOTE_ORIGINS=https://astra.example.com
```

## 12. 排障

先跑 `make remote-check`（电脑端），再看下表。

| 现象 | 最可能的原因 | 怎么确认 / 怎么修 |
|---|---|---|
| 手机上是**黑屏**，一直没画面 | 显示器休眠；或没授予「屏幕录制」 | 见 §8。`make remote-check` 里「抓屏」一行字节数很小（<8KB）就是全黑。唤醒屏幕，去 系统设置 → 隐私与安全 → 屏幕录制 勾选后**重启服务** |
| **能看不能控**，点击没反应 | 缺「辅助功能」权限 | 同上「注入」一行；`admin-cli remote capabilities` 的 `input.reason` 写着 `-10004`。勾选后重启服务 |
| **连不上**：页面打开就是断线 / WS 403 | 反向代理没配 WebSocket 升级；或来源不在白名单 | 反代要转发 `Upgrade` / `Connection` 头（Nginx 用 `proxy_set_header Upgrade $http_upgrade; proxy_http_version 1.1;`）。跨域访问要设 `ASTRA_REMOTE_ORIGINS=https://你的域名` |
| **帧率低**（1–5fps 且画面糊） | 显示器休眠导致抓屏退避；或网络太差 | 先看状态条 fps 与「掉帧」；再看电脑上是不是熄屏了（§8）。降一档画质，或缩小窗口 |
| **会话一直 pending**，手机点不动 | 没在电脑上确认接管 | 控制面「远程桌面」面板点「允许」，或 `admin-cli remote approve <sessionID>` |
| 所有接口 **503** | 全局急停 / 能力未装配 | `ASTRA_REMOTE_DISABLED` 是否为 1；看服务启动日志里「远程桌面已启用」是否出现 |
| 页面顶部显示 **`中继断开`**（relay 未连接） | 电脑端没连上 relay：地址写错、token 不一致、relay 没起、证书不被信任 | ①`/v1/remote/tunnel` 或顶部状态条里的 `lastError`；②电脑端启动日志有没有「外网中继已启动」；③两边 token 用 `shasum -a 256` 比哈希，别肉眼比；④自签证书本机验证时临时开 `ASTRA_RELAY_INSECURE=1`（生产别开）；⑤`curl https://relay/_relay/healthz` 看 devices 是不是 0 |
| **手机连上了，但画面不动**（一直停在最后一帧） | 多半是 WebRTC 协商失败已回退，而回退后 WS 帧又被网络卡住；也可能是电脑那侧画面真的没变化（静止画面只发 2 秒心跳帧） | ①看顶部链路标签：显示 `中继(WS)` 就是已回退（悬停能看到原因），这本身不影响出图；②等 2–3 秒看是否补帧；③状态条 fps 为 `—` 且「掉帧」不动 → 网络断，看会话列表里 fps；④都不行就断开重连，会重建 WebRTC 与一次性票据 |
| **404** `设备或会话不存在` | id 写错，或那台设备属于别的账号 | `admin-cli remote devices` / `remote sessions` 重新取 id；跨账号看不到是设计如此 |

常用对照命令：

```bash
admin-cli remote capabilities          # 三层能力 + 缺什么 + 怎么修
admin-cli remote devices               # 设备是否在线 / 是否被吊销
admin-cli remote sessions --limit 20   # 有没有会话在跑、帧率与掉帧
admin-cli remote end <sessionID>       # 急停

curl -s https://relay.example.com/_relay/healthz   # relay 在线设备数（无需鉴权）
```
