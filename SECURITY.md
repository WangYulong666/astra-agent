# 安全策略

## 报告漏洞

**不要开 public issue。** 用 GitHub 的私密报告：
仓库页 → Security → Report a vulnerability（Security Advisories）。

如果那个入口不可用，邮件联系维护者（见仓库 commit 作者邮箱），标题前缀 `[astra-agent security]`。

请在报告里带上：影响版本、复现步骤、影响面（能否读到别人的会话/密钥/宿主机文件）。
我们会在 72 小时内确认收到，修好并发布后再公开细节。

## 支持范围

只对最新 tag 与 `main` 分支的最新提交提供修复。旧版本请先升级。

## 这套系统的信任边界（读代码前必须理解）

| 边界 | 含义 |
|---|---|
| **沙箱是唯一的执行边界** | agent 在沙箱里跑，越界要审批。宿主机进程本身不隔离 agent |
| **真 key 不进沙箱** | 上游 key 只在控制面内存；沙箱拿到的是短期虚拟 key（默认 2h），经网关转发并记账 |
| **审批是安全边界的一部分** | `policy=allow-all` 等于关掉人审这一层；公网部署务必用 `ask` 或更严 |
| **JWT 密钥必须持久化** | `<dataDir>/jwt-secret`（0600）。换密钥 = 全端下线；泄露 = 任何人可伪造会话 |
| **`requireAuth=false` 是开发模式** | 兼容模式跳过所有认证，**绝不要**在公网开 |

## 已知的设计取舍（不是漏洞，但会被误报）

1. **`local` provider 的写限制靠 `sandbox-exec`（macOS）/ `bwrap`（Linux）**，本质是进程级约束，强度低于 microVM。要强隔离请用 `cube` provider。
2. **虚拟 key 在沙箱内可见**：它本来就给 agent 用，风险被限制为「2 小时内、经网关、有额度记账、可吊销」。
3. **审批超时默认按拒绝处理**：宁可任务失败，不给「无人看守时自动放行」。
4. **桌面端只听 127.0.0.1**：要跨设备访问请用服务器部署或反代，不要把桌面端暴露出去。

## 仓库里哪些东西永远不会被提交

`.env*`（保留 `.env.example`）、`*.pem`/`*.key`/`*.p12`、`kubeconfig*`、`secrets/`、`*credentials*`（**但 `*.go` 例外**——源码不是凭据）、`/bin/`、`/dist/`、`node_modules/`。

密钥类文件只存在于数据目录（默认 `./data` 或 `~/Library/Application Support/Astra/data`），由运行时生成。

## 依赖

上游源码锁定在 `third_party/upstream.lock.toml`（含 license 字段），拉取走 `make sync`。
平台内核组件只接受 OSI 认可的开源许可（Apache-2.0 / MIT / BSD）；BSL / RSALv2 / SSPL / AGPL 一律不引入。
