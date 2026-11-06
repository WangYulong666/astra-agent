// Package sandbox 抽象「Agent 跑在哪儿」。
//
// 存在的理由：上层（worker / gateway）不应该知道底层是 KVM microVM、Apple
// 容器还是宿主机上的一个进程。生产用 CubeSandbox，本机开发用 local，
// 两者对上面暴露同一组语义。
//
// 设计边界——只抽象我们真正用到的能力，不照抄 e2b 的 API 面：
//
//	需要：起沙箱、拿 ACP 端点、快照、暂停、销毁、接管（failover）
//	暂不需要：PTY、端口暴露、文件 RPC、模板构建（M2+ 再加，见 types 里的 TODO）
//
// 一个约束贯穿全部实现：**一个会话 = 一个沙箱 = 一个 agent 进程 = 一条 ACP 连接**。
// 所以 Create 的 ID 由调用方按会话维度给出，provider 不得自行编造。
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 统一错误。上层靠这些分支决定「重建沙箱」还是「只重连」。
var (
	// ErrNotFound 沙箱不存在（已被销毁或从未创建）。
	ErrNotFound = errors.New("sandbox: not found")
	// ErrNotRunning 沙箱存在但进程/VM 已停——可以 Resume，不必重建。
	ErrNotRunning = errors.New("sandbox: not running")
	// ErrUnsupported provider 不支持该操作（如 Cube 的本地快照）。
	ErrUnsupported = errors.New("sandbox: operation not supported")
)

// State 沙箱状态。与 e2b 的 running/paused 语义对齐，便于将来换 provider 不改上层。
type State string

const (
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
	StateUnknown State = "unknown"
)

func (s State) String() string { return string(s) }

// Endpoint 是 worker 连 agent 需要的一切。
//
// 为什么带 Token：acp-proxy 拒绝无凭据连接（见 cmd/acp-proxy）。token 由 provider
// 生成并只在本机落盘，不经过控制面传递，避免「控制面记明文凭据」。
type Endpoint struct {
	URL   string // 形如 ws://127.0.0.1:40123/acp
	Token string // Bearer token；空表示该 provider 不做鉴权（仅 local 允许）
}

// NetworkPolicy 描述「出网强制边界」：用 Seatbelt 把出网收窄到本机代理端口。
//
// 这是双层出网边界里的第 1 层（强制）：
//   - 本结构驱动 Seatbelt，只放行连本机代理端口、DNS 的 unix-socket、本机 inbound/bind。
//   - 域名 allow/deny/ask、审计、token 注入仍由 egress 代理（第 2 层）执行。
//     Seatbelt 只能按数值 IP/端口匹配，看不到域名，两层职责不重叠。
//
// 形状由主控在 runner 侧构造后塞进 AgentSpec；本包不读环境变量、不改 runner。
//
// ⚠️ 默认必须关闭。规则一旦写错（或代理没起在该端口），代价是「所有 agent 连不上
// 模型」。开启与否由主控按部署形态决定，sandbox 只负责忠实执行传进来的配置。
type NetworkPolicy struct {
	// Enabled 总开关。false 时生成的 profile 与未启用本能力时逐字节相同。
	Enabled bool
	// ProxyAddr 出网代理地址，形如 "127.0.0.1:17890"。
	// 生成规则时按端口放行；host 会归一化（见 local.go 的 sbplAddr）。
	ProxyAddr string
	// ExtraAllow 额外放行的 "host:port"，如本机 LLM 网关 "127.0.0.1:7788"。
	// 逐条校验，非法值忽略并记日志。
	ExtraAllow []string
}

// AgentSpec 描述沙箱里要跑什么 agent。
//
// 镜像/容器方案会忽略 Command 而用模板里预置的入口，local 方案则真的按 Command 起进程。
// 这个差异是刻意的：生产镜像里 agent 与 acp-proxy 的启动方式是镜像契约，不该由请求端随意改。
type AgentSpec struct {
	// Command agent 启动命令，如 ["dsh","--profile","acp"]。
	Command []string
	// Home agent 的会话持久化根目录（dsh 对应 DSH_HOME）。
	// 跨进程/跨重启的 session/resume 全靠它，所以必须在沙箱生命周期之外持久。
	Home string
	// Env 追加环境变量。
	Env map[string]string
	// SelfSandbox 表示「agent 自带文件级沙箱，外层不要再套」。
	//
	// 为什么需要这个开关：macOS 的 sandbox_apply 在已被沙箱化的进程里直接返回
	// Operation not permitted——**沙箱不能嵌套**。所以「平台套一层 + agent 自己再套一层」
	// 必然失败，表现为 agent 的 bash 工具全部报 sandbox backend unusable。
	// 强制边界只能有一层，由谁提供必须显式声明：
	//   SelfSandbox=false（默认）→ 平台负责（local provider 用 sandbox-exec 包裹）
	//   SelfSandbox=true          → agent 负责（dsh 的 workspace-write Seatbelt），
	//                               平台不再包裹，否则两层互斥
	// 这不影响生产形态：cube 的 microVM 是边界，容器内 agent 本来就跑在「无约束」态。
	SelfSandbox bool
	// ProxyBin acp-proxy 二进制路径；空表示与 provider 同目录/走 PATH。
	ProxyBin string
	// NetworkPolicy 出网强制边界配置（可选）。nil 或 Enabled=false 表示不启用，
	// profile 与现状完全一致。
	//
	// 由主控构造；会持久化进 meta.json，使 pause→resume（Resume 只带 Command 重启）
	// 后边界不静默消失。spawn 一律以 meta 里落盘的值为准，不读这里的字段。
	NetworkPolicy *NetworkPolicy
}

// CreateRequest 起一个沙箱。
type CreateRequest struct {
	// ID 调用方指定的稳定标识（按会话维度）。provider 不得改写。
	ID string
	// Template 镜像/模板名。local provider 忽略此字段。
	Template string
	Agent    AgentSpec
	// Workspace 宿主侧工作目录。local/apple 直接使用；cube 只用于上传初始内容。
	// 空则由 provider 分配默认目录。
	Workspace string
	// Token acp-proxy 的 Bearer token。空则由 provider 生成。
	// 由调用方传入的好处是：日志/排障时无需去翻 provider 的私有存储。
	Token string
	// Timeout 沙箱存活上限，0 表示不限（local 默认不限，cloud 默认由平台决定）。
	Timeout time.Duration
	// SnapshotFrom 从既有快照派生（模板/分支场景）；空表示全新。
	SnapshotFrom string
}

// Info 沙箱的可观测状态。用于 CLI 列表与 worker 健康检查。
type Info struct {
	ID        string
	Provider  string
	State     State
	Endpoint  Endpoint
	Workspace string
	PID       int // local: acp-proxy 的 PID；container/cube: 0
	CreatedAt time.Time
	// Detail provider 私有信息（容器名、VM 名、远端 id 等），仅用于日志与调试。
	Detail string
}

// Sandbox 是一个活着的沙箱句柄。
//
// 语义约定：
//   - Close 只释放本地句柄，**不销毁**沙箱（worker 重启后还能 Acquire 回来）
//   - Kill 才销毁
//   - 任何方法在 ctx 取消时应尽快返回，不得留下半初始化状态
type Sandbox interface {
	ID() string
	Info(ctx context.Context) (Info, error)
	Endpoint() Endpoint

	// Exec 在沙箱内跑命令。M1 的「取产出物」「模板预检」要用。
	// TODO(M1): 目前只有 local 实现；容器/Cube 走 envd 时需要先 buf generate proto。
	Exec(ctx context.Context, cmd []string, opts ExecOpts) (ExecResult, error)

	// Snapshot 把当前 workspace 冻结成可复用的基线，返回快照名。
	// local 用 APFS clonefile（毫秒级）；cube 走模板；容器走 commit。
	Snapshot(ctx context.Context, name string) (string, error)

	// Pause 保留状态地停下（可 Resume）。Cube 的 pause 是内存快照，
	// local 退化为「停进程 + 保留目录」，语义对齐但代价不同，文档已注明。
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error

	Close() error
	Kill(ctx context.Context) error
}

// ExecOpts 命令执行选项。
type ExecOpts struct {
	Dir     string
	Env     map[string]string
	Stdin   []byte
	Timeout time.Duration
}

// ExecResult 命令执行结果。
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// WorkspaceLocator 是 provider 的可选能力：给出某个沙箱在**宿主机上可直接读**的
// workspace 绝对路径。
//
// 只有「工作区本来就在本机」的 provider 能实现（local）。容器与 Cube 的工作区在
// 另一层文件系统里，要经 Exec 或 envd 才能读——那种情况不实现这个接口，
// 上层据此返回「不支持」，而不是给一个看起来能用、读出来却是空的路径。
type WorkspaceLocator interface {
	HostWorkspace(id string) (string, error)
}

// Provider 是沙箱后端。
type Provider interface {
	// Name provider 标识，写进 Info 与日志。
	Name() string

	// Create 起沙箱。已存在同 ID 时应返回可用句柄而不是报错（幂等）。
	Create(ctx context.Context, req CreateRequest) (Sandbox, error)

	// Acquire 接管已存在但当前无句柄的沙箱——worker 重启 / failover 用。
	// 不存在的返回 ErrNotFound，存在但已停的返回 ErrNotRunning（提示调 Resume）。
	Acquire(ctx context.Context, id string) (Sandbox, error)

	// Destroy 销毁沙箱，**不要求它当前是活的**。
	//
	// 为什么单独有这个而不用 sandbox.Kill：清理已 stopped/paused 的残留是常态
	// （异常退出、机器重启），而那些状态下 Acquire 会返回 ErrNotRunning、拿不到句柄。
	// 没有这个方法的话，清理逻辑只能假装成功——或者直接报错把残留留给用户。
	Destroy(ctx context.Context, id string) error

	// List 枚举本 provider 下的全部沙箱。
	List(ctx context.Context) ([]Info, error)
}

// New 按名字构造 provider。
//
// 刻意不用 init() 注册表：provider 集合是封闭的，显式 switch 更容易在编译期发现遗漏，
// 也让调用方能读到「为什么这个名字不合法」。
func New(name string, opts Options) (Provider, error) {
	switch name {
	case "local":
		return NewLocal(opts), nil
	case "applecontainer", "apple":
		return NewAppleContainer(opts), nil
	case "cube", "cubesandbox":
		return NewCube(opts), nil
	default:
		return nil, fmt.Errorf("sandbox: 未知 provider %q（可选 local / applecontainer / cube）", name)
	}
}

// Options 全部 provider 的公共配置。各 provider 只读自己关心的字段。
type Options struct {
	// Root 本地状态根目录。local 落 workspace/home/meta，apple container 落映射表。
	Root string
	// ProxyBin acp-proxy 二进制路径。
	ProxyBin string
	// ProxyImage 容器方案里跑 acp-proxy 的镜像。
	ProxyImage string
	// CubeAPI CubeSandbox 控制面地址（如 http://cube-api.example.com:3000）。
	CubeAPI string
	// CubeToken CubeAPI 的鉴权凭据。
	CubeToken string
	// Verbose 打开 provider 内部日志。
	Verbose bool
}
