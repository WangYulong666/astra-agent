# 外网中继（astra-relay）运维手册

> 目标读者：要在公网机器上把中继跑起来、并且要能在出事时定位的人。

## 1. 为什么需要 relay

远程控制要求「手机在任何网络下都能连上家里那台电脑」，但家里电脑通常在 NAT 后面：
没有公网 IP、不想开端口、更不能让用户去改路由器。所以链路反过来建——**电脑主动出站**
连到一台公网 relay 上并保持长连接，手机则去连 relay 的 `/t/<device>/...`，
relay 把字节原样转给那条已经建好的连接。用户侧要做的事情只有「填一个 URL + 一个 token」，
不需要域名、不需要证书、不需要端口映射。relay 本身很薄：一条 WSS 上跑
[smux](https://github.com/xtaci/smux) 多路复用，每个 HTTP 请求 / 每个 WebSocket 升级
各占一条逻辑流，relay 不解析业务、不落盘、不缓存。

```
手机 ──HTTPS/WSS──▶ relay（公网，astra-relay）◀──出站长连接（WSS）── 电脑（astra-server）
                     /t/{device}/...              smux 多路复用，每请求一条流
```

## 2. 部署前置

| 需要 | 说明 |
|---|---|
| 一台公网机器 | 1C1G 足够；出口带宽决定画面帧率上限 |
| 一个域名 | 例 `relay.example.com`，A/AAAA 记录指向这台机器 |
| 一张证书 | Let's Encrypt 即可。**要么** relay 自己终结 TLS（`-tls-cert/-tls-key`），**要么** 由 nginx/caddy 终结后明文回源到 relay |
| 一个共享 token | `openssl rand -hex 32`。它同时是「设备注册凭据」和「票据签名密钥」 |

端口：直连方式开放 `9443/tcp`（或你选的端口）；反代方式只开放 `80/443`，
relay 监听 `127.0.0.1:9443`。

## 3. 起服务

编译：

```bash
make build            # 产物在 bin/astra-relay
# 或
go build -o bin/astra-relay ./cmd/astra-relay
```

**方式 A：relay 自己终结 TLS（推荐，链路最短）**

```bash
bin/astra-relay \
  -addr :9443 \
  -token "$(cat /etc/astra/relay-token)" \
  -tls-cert /etc/letsencrypt/live/relay.example.com/fullchain.pem \
  -tls-key  /etc/letsencrypt/live/relay.example.com/privkey.pem \
  -public-url https://relay.example.com \
  -max-streams 128 \
  -idle-timeout 90s
```

**方式 B：nginx/caddy 终结 TLS，relay 只监听回环明文**

```bash
bin/astra-relay -addr 127.0.0.1:9443 -token "$(cat /etc/astra/relay-token)" \
  -public-url https://relay.example.com
```

启动后会打印一段横幅，包含：监听地址（含实际端口）、是否 TLS、是否调试模式、公网 URL、
以及手机访问地址和设备端应填的 `ASTRA_RELAY_URL`。**没给 token 时会打印一段醒目的危险告警**，
见 §7。

参数与环境变量：

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-addr` | — | `:9443` | 监听地址；反代场景用 `127.0.0.1:9443` |
| `-token` | `ASTRA_RELAY_TOKEN` | 空 | 设备注册令牌；为空=调试模式（只允许回环） |
| `-public-url` | — | 空 | 只用于日志/横幅展示 |
| `-tls-cert` / `-tls-key` | — | 空 | **必须成对给出**，否则启动即报错 |
| `-max-streams` | — | `128` | 单设备并发流上限，超过返回 429 |
| `-idle-timeout` | — | `90s` | 隧道空闲断开阈值（smux keepalive），不得小于 10s |

## 4. systemd

```ini
# /etc/systemd/system/astra-relay.service
[Unit]
Description=Astra Relay (reverse tunnel for remote desktop)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=astra
Group=astra
# token 放单独的文件里（0600），别写进 unit —— unit 内容会被 systemctl cat 出来
EnvironmentFile=-/etc/astra/relay.env
ExecStart=/usr/local/bin/astra-relay \
  -addr 127.0.0.1:9443 \
  -public-url https://relay.example.com \
  -max-streams 128 \
  -idle-timeout 90s
Restart=always
RestartSec=2s
# 优雅退出：SIGTERM 后 relay 会先关隧道再关 HTTP
KillSignal=SIGTERM
TimeoutStopSec=15

# 加固（relay 不需要写盘）
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadOnlyPaths=/etc/astra

[Install]
WantedBy=multi-user.target
```

注意 `Token` 直接用 `-token` 传更简单，但会出现在 `ps` 里；建议写进 `EnvironmentFile`：

```bash
# /etc/astra/relay.env
ASTRA_RELAY_TOKEN=把 openssl rand -hex 32 的输出粘这里
```

```bash
sudo install -m 0600 -o astra -g astra /dev/null /etc/astra/relay.env
sudo systemctl daemon-reload && sudo systemctl enable --now astra-relay
journalctl -u astra-relay -f
```

## 5. 反代 WSS 的三个坑

隧道是 **WebSocket**，不是普通 HTTP。反代最容易在这里静默失败（握手 200/400，或者连上几秒就断）。

**nginx**

```nginx
location / {
    proxy_pass http://127.0.0.1:9443;
    proxy_http_version 1.1;

    # 必须：缺这两行，升级请求会被当成普通请求，设备端表现为 400/不发 Upgrade
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";

    # 必须：大于 relay 的 -idle-timeout，否则空闲隧道被 nginx 先掐断，
    # 设备端会看到周期性重连（日志里 90s 一次，很规律）
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
}
```

**caddy**

```caddyfile
relay.example.com {
    reverse_proxy 127.0.0.1:9443 {
        # caddy 默认就透传 Upgrade/Connection，但超时要显式放开
        transport http {
            read_timeout 1h
            write_timeout 1h
        }
    }
}
```

三条硬要求，缺一不可：

1. `Upgrade` / `Connection: upgrade` 必须透传（否则 `/relay/tunnel` 握手失败）。
2. `proxy_read_timeout`（或等价配置）必须 **大于** relay 的 `-idle-timeout`；
   否则反代先断，隧道每 N 秒重连一次，症状是「能用但一直抖」。
3. 不能开启响应缓冲/压缩（`proxy_buffering off`），否则远程桌面的帧会被攒起来，
   表现为画面一顿一顿。

顺带：反代回源到 `127.0.0.1:9443` 时，relay 看到的来源是回环地址。
**如果你在反代上开了「relay 的 token 为空」的调试模式，任何人都能注册设备**（见 §7）。

## 6. 设备端怎么配

设备端就是家里的 `astra-server`，用环境变量接入（见 `internal/app/app.go` 的 `startTunnel`）：

```bash
# 必填：relay 地址，ws:// 或 wss://
ASTRA_RELAY_URL=wss://relay.example.com

# 必填（relay 有 token 时）：与 relay 的 -token 完全一致
ASTRA_RELAY_TOKEN=8f3c...（与 relay 同一串）

# 选填：设备 id。不填则取 hostname（小写、空格换 -）
# 它决定手机访问路径 /t/<device>/，所以必须「不可猜」且唯一
ASTRA_RELAY_DEVICE=mac-mini-a1b2c3

# 仅本机自签证书验证时使用：跳过证书校验（生产不要开）
# ASTRA_RELAY_INSECURE=1
```

隧道会把每条流接到控制面自己的监听地址（`astra-server -addr`，默认 `127.0.0.1:7788`），
所以控制面只要正常起在本机就行，不需要对外暴露端口。

启动日志出现 `外网中继已启动：wss://... （设备 mac-mini-a1b2c3）` 即为成功；
`/v1/remote/tunnel` 接口（见 `internal/api`）会返回 `connected / since / reconnects / streams / lastError`。

**设备 id 的选择很重要**：

- 它出现在公网 URL 里，等于一个「账号名」；用 `mac-mini` 这种可猜的名字，
  攻击者虽然没有 token 也能不断试探 `/t/mac-mini/...`（只会拿到 503，但会污染日志）。
- 正式部署应该在配对流程里下发随机 id（例 `dev_7c1f9a...`），hostname 只适合调试识别。
- 字符集限制：`A-Z a-z 0-9 . _ -`，首字符必须是字母或数字，最长 64。

## 7. 手机怎么访问

```
https://relay.example.com/t/<device>/...
```

例：

```bash
# 控制面元信息
curl https://relay.example.com/t/mac-mini-a1b2c3/v1/meta

# 远程桌面的 WebSocket（前端把 base 换成 /t/<device> 即可）
wss://relay.example.com/t/mac-mini-a1b2c3/v1/remote/ws?session=...
```

relay 会把 `/t/<device>` 剥掉，转发给设备上的 `/<rest>`，并带上：

- `X-Forwarded-Prefix: /t/<device>`（下游生成绝对链接时不能错）
- `X-Forwarded-For: <手机真实 IP>`（客户端自带的这个头会被丢弃，防伪造）

状态码语义（**故意区分**，方便运维）：

| 状态 | 含义 |
|---|---|
| `503 {"error":"设备未连接"}` | 设备没上线 / 刚掉线。**不是**路径写错 |
| `404 {"error":"路径应为 /t/{device}/..."}` | 路径确实写错了 |
| `429 {"error":"设备并发流超限"}` | 该设备在途请求超过 `-max-streams` |
| `502 {"error":"隧道不可用"}` | 流已建立但设备端本地控制面连不上（比如 astra-server 挂了） |
| `401 {"error":"票据无效或已过期"}` | 设备注册票据错 → 看 §8.2 |
| `200 {"ok":true,"devices":N}` | `/_relay/healthz`，无需鉴权，可直接给 LB 探活 |

## 8. 排障 5 条

### 8.1 手机拿到 503「设备未连接」

```bash
curl -s https://relay.example.com/_relay/healthz        # devices 是不是 0
journalctl -u astra-relay -n 100 | grep 设备
```

- `devices:0` → 设备没连上。看设备端日志：`外网中继已启动` 有没有出现；
  有没有 `隧道重连失败` 的循环（`LastError` 里能看到 HTTP 状态码）。
- 设备 id 拼错也表现为 503（路径里那一段必须和 `ASTRA_RELAY_DEVICE` 完全一致）。
- 设备刚掉线时会有 1 秒级窗口：relay 监听的是底层 WSS 的读写错误，
  拔网线（无 FIN）靠 keepalive，最坏等一个 `-idle-timeout`。

### 8.2 设备端 401（票据无效/已过期）

票据是 `sig = HMAC-SHA256(token, device + "|" + ts)`，`ts` 是 unix 秒，**允许 ±120s 偏差**。

- `设备端时钟不准` 是最常见原因（虚拟机休眠、树莓派无 RTC）：跑 `timedatectl` / `date -u`，
  开 NTP。
- **token 不一致**：relay 用 `-token`（或 `ASTRA_RELAY_TOKEN`），设备用 `ASTRA_RELAY_TOKEN`，
  两边必须是同一串。注意别把换行/引号/空格带进去（`"$(cat token)"` 里若有换行就是另一个 token）。
- 401 只在**注册**时出现；已建立的隧道不会因为票据过期而断（票据只在握手用一次，
  这也是为什么窗口只需要 120s）。

### 8.3 反代没透传 Upgrade

症状：设备端报 `expected handshake response status code 101 but got 400/200`，
或者浏览器里 `/t/<device>/v1/remote/ws` 一直 pending 后 504。

- nginx：`proxy_http_version 1.1` + `proxy_set_header Upgrade $http_upgrade` +
  `proxy_set_header Connection "upgrade"` 三行缺一不可。
- caddy：默认透传，多半是超时/缓冲问题，检查 `read_timeout`。
- 云厂商 LB（SLB/CLB）：确认监听器是 **TCP 或 HTTP(S) 且开启 WebSocket**，
  七层 LB 的「请求超时」要调到 ≥ 3600s。

### 8.4 证书问题

- `x509: certificate signed by unknown authority` → 设备连的是自签证书。
  要么换成正式证书，要么临时 `ASTRA_RELAY_INSECURE=1`（仅在受控网络）。
- `x509: certificate is valid for X, not Y` → `ASTRA_RELAY_URL` 里的域名和证书 SAN 不一致
  （常见于用 IP 直连）。
- 证书续期后 relay **不会热加载**，必须 `systemctl reload-or-restart astra-relay`
  （或让 nginx/caddy 终结 TLS，这样续期与 relay 无关）。

### 8.5 token 不一致 / 忘记 token

- 症状一：设备稳定 401。
- 症状二：设备能连上，但 relay 日志里 `设备 xxx 重复注册，关闭旧会话` 反复出现——
  两台机器用了同一个 `ASTRA_RELAY_DEVICE`（它们会互相顶号）。
- 排查：

  ```bash
  sudo systemctl cat astra-relay | grep -i token
  sudo grep -c . /etc/astra/relay.env      # 确认文件非空且只有一行
  # 设备端
  echo -n "$ASTRA_RELAY_TOKEN" | shasum -a 256   # 两边比对哈希，别肉眼比长串
  ```

## 9. 安全边界（必须读完再上线）

**relay 不校验平台 JWT。** 它只做两件事：(a) 设备注册时校验 HMAC 票据；
(b) 未知设备返回 503。所有业务鉴权——登录态、会话归属、审批、急停——仍然在 origin
（家里的 `astra-server`）执行，relay 从不解析、也看不懂这些内容。

这意味着：

- **relay 能看到什么**：谁在什么时候连了哪个设备 id（IP、时间、字节数、请求路径与
  状态码——只要你自己加日志）；因为 TLS 在 relay 终结，**relay 是明文可见的管道**：
  如果 origin 只跑明文 HTTP，那么 relay 上（以及任何能读 relay 内存/抓 relay 网卡的人）
  可以看到完整的请求/响应体，包括 Bearer token 和远程桌面画面。
- **relay 不能看到什么**：平台侧的 JWT 是否有效（没有密钥，也无从校验）、
  谁有权控制哪台电脑、业务数据（不落盘）。它也**不能**伪造 origin 的响应给手机，
  除非主动做手脚——所以 relay 必须是你信任的机器。
- **要得到端到端加密**：在 origin 上再套一层 TLS（`https://127.0.0.1:8443` 作为上游），
  隧道里走的就是密文；或者干脆只在局域网点对点用 WebRTC（v2 的另一条管道）。
- **token 是设备注册的唯一凭据**：拿到 token 的人可以注册任意设备 id、接管
  `/t/<id>/` 的流量（相当于中间人）。所以要长随机、只放 `0600` 文件、能轮换就轮换
  （轮换时先后改 relay 与设备，中间会有 401 窗口）。
- **调试模式（token 为空）只允许 127.0.0.1 连入**，判断依据是连接的真实来源
  `RemoteAddr`（不看 `X-Forwarded-For`，那个头谁都能写）。但请注意：**如果 token 为空、
  且前面挂了反代**，relay 看到的来源就是回环 → 检查会通过 → 任何人只要能访问反代，
  就能注册设备。所以：要么配 token，要么确保 relay 端口只有你自己能访问。

`/_relay/healthz` 不需要鉴权（只返回在线设备数量），可以直接给 LB 探活。
