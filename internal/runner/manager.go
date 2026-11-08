// Package runner 是控制面的执行体：把「一次 prompt」落到「一个沙箱里的一条 ACP 对话」。
//
// 职责边界刻意收窄——它不做 HTTP、不做鉴权、不写 UI，只管：
//
//  1. 会话 ↔ 沙箱 ↔ ACP 会话 的绑定与恢复
//  2. session/update → 事件（合并高频增量后落库）
//  3. 权限请求 → 按策略回复（并留痕）
//  4. 取消与终态归档
//
// 这样 M1 的 HTTP 层可以是单进程直连，M2 换成 worker + 消息队列时这层几乎不用改。
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/acp"
	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/insight"
	"github.com/WangYulong666/astra-agent/internal/obs"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// Config 装配 runner。
type Config struct {
	Store    store.Store
	Provider sandbox.Provider
	Hub      *eventbus.Hub

	// Agent 沙箱内要跑的 agent。空则用 sandbox.DefaultAgentCommand()。
	Agent  sandbox.AgentSpec
	Policy PermissionPolicy

	// CoalesceWindow 高频增量的合并窗口。0 用 50ms。
	CoalesceWindow time.Duration
	// ConnectTimeout 连沙箱与 ACP 握手的超时。0 用 20s。
	ConnectTimeout time.Duration
	// ApprovalTimeout 人工审批的等待上限。0 用 10 分钟。
	ApprovalTimeout time.Duration
	Logger          *log.Logger

	// Creds 会话级模型凭据签发器。nil 表示不做替换——此时沙箱直接继承
	// Agent.Env 里的上游 key（本地开发用），生产必须配。
	//
	// 装配语义：非 nil 时，创建会话前调 IssueForSession 拿到 env 覆盖 Agent.Env。
	// 签发失败**直接让创建失败**——静默放行会得到一个能起来但永远 401 的 agent，
	// 用户看到的只是「模型不回话」，排查成本远高于当场报错。
	Creds CredentialIssuer

	// ── 出网管控与第三方凭据（可选）──────────────────────────
	// 由 app 在 runner.New 之前装配好。nil 表示未启用：沙箱保持直连，
	// 也不注入任何额外环境变量——能力关着时不能改变既有行为。
	Egress     egress.Controller
	Connectors connector.Manager
	// EgressBypass 是 NO_PROXY 的值，默认空（严格，全部走代理）。
	// 本地开发若 agent 通过 127.0.0.1 的 LLM 网关取模型，必须把该地址
	// 填进来，否则代理的私网拦截会把网关一起挡掉（默认 deny）。
	EgressBypass string
	// NetworkLock 打开 Seatbelt 的「只准连本机出网代理」规则。
	// 默认 false：规则写错或代理没起会让所有 agent 断网，必须由部署方显式开启。
	// 只在 Egress 已启用时生效，否则 app 层会忽略并告警。
	NetworkLock bool

	// 自闭环：沙箱内原子能力所需的运行时上下文。
	// 这些值在 runner.New 之后才齐（控制面实际监听地址、insights store），
	// 所以用可变的 RuntimeHints 传指针，app 装配完再 Set，不改 Config 值语义。
	Hints *RuntimeHints
	// SandboxKeyIssuer 签发会话级受限凭据。nil 表示不注入（兼容模式）。
	SandboxKeyIssuer SandboxKeyIssuer
	// InsightReplay 是否把上一轮失败诊断作为 prompt 前缀回灌。
	InsightReplay bool
}

// SandboxKeyIssuer 是 auth.Service 的子集。用窄接口是为了让 runner 不依赖
// 具体鉴权实现，也方便测试塞假签发器。
type SandboxKeyIssuer interface {
	IssueSandboxKey(tenantID, userID, sessionID string, ttl time.Duration) (string, time.Time, error)
}

// RuntimeHints 持有「装配后期才确定」的运行时信息。
type RuntimeHints struct {
	mu              sync.RWMutex
	insights        insight.Store
	controlPlaneURL string
	cliBin          string
}

func NewRuntimeHints() *RuntimeHints { return &RuntimeHints{} }

func (h *RuntimeHints) Set(ins insight.Store, url, cli string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.insights, h.controlPlaneURL, h.cliBin = ins, url, cli
	h.mu.Unlock()
}

func (h *RuntimeHints) SetURL(url string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.controlPlaneURL = url
	h.mu.Unlock()
}

func (h *RuntimeHints) Snapshot() (insight.Store, string, string) {
	if h == nil {
		return nil, "", ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.insights, h.controlPlaneURL, h.cliBin
}

// Manager 管理全部会话。并发安全。
type Manager struct {
	cfg Config
	log *log.Logger

	baseCtx context.Context
	cancel  context.CancelFunc

	mu    sync.Mutex
	binds map[string]*binding
	wg    sync.WaitGroup

	// 人工审批注册表：key = runID/toolCallID。
	// 用独立锁而不是复用 mu：审批会阻塞几十秒到几分钟，拿住会话锁等于把
	// 整个控制面的会话操作全堵死。
	apprMu sync.Mutex
	apprs  map[string]*approvalWait
	// apprStore 是审批的共享存储。与 cfg.Store 分开存一份：New 时已断言
	// 它存在，之后每次用都不必再类型断言。
	apprStore store.ApprovalStore

	// gate 处理「取消先于 prompt 到达 agent」的竞态，见 cancel.go。
	gate *cancelGate
}

// binding 是一个会话的运行时绑定。持有 ACP 客户端，并在会话内串行化 prompt。
type binding struct {
	m    *Manager
	sess store.Session
	sem  chan struct{} // 容量 1：一个 ACP 会话同时只能有一个进行中的 prompt

	mu     sync.Mutex
	client *acp.Client
}

// New 构造 Manager。不启动后台任务。
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("runner: 缺少 Store")
	}
	// 审批必须落共享存储，否则进程重启 / 多副本下等待方会永久挂起。
	// 这里 fail-fast，而不是等到第一次审批才 panic 类型断言。
	apprStore, ok := cfg.Store.(store.ApprovalStore)
	if !ok {
		return nil, errors.New("runner: Store 未实现 store.ApprovalStore（审批无法持久化）")
	}
	if cfg.Provider == nil {
		return nil, errors.New("runner: 缺少 Provider")
	}
	if cfg.Hub == nil {
		cfg.Hub = eventbus.New()
	}
	if cfg.Policy.Name == "" {
		cfg.Policy = WorkspaceWrite()
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 20 * time.Second
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = 10 * time.Minute
	}
	if len(cfg.Agent.Command) == 0 {
		cfg.Agent.Command = sandbox.DefaultAgentCommand()
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.New(os.Stderr, "runner ", log.LstdFlags)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, log: lg, baseCtx: ctx, cancel: cancel,
		binds: map[string]*binding{}, apprs: map[string]*approvalWait{},
		apprStore: apprStore, gate: newCancelGate()}, nil
}

// Hub 暴露给 HTTP 层做订阅。
func (m *Manager) Hub() *eventbus.Hub { return m.cfg.Hub }

// CreateSessionOptions 建会话的参数。
type CreateSessionOptions struct {
	// ID 指定会话 id（测试用）；空则生成。
	ID string
	// TenantID / UserID 是归属。接入认证后必填，API 层按它过滤；
	// 兼容模式（无认证）下为空，此时所有查询不做归属校验。
	TenantID string
	UserID   string
	// ParentSessionID 非空表示这是某个父会话派生的子 agent（多 agent 协作）。
	// 由服务端从父 run 继承，不接受 HTTP 请求体直接传。
	ParentSessionID string
	// Cwd 沙箱内的初始工作目录；空则用沙箱 workspace。
	Cwd string
	// Token acp-proxy 的凭据；空则由 provider 生成（生产里控制面不该知道明文）。
	Token string
}

// CreateSession 起沙箱、握手、建 ACP 会话，并落库。
//
// 失败时要把已经起来的沙箱销毁，否则会留下「沙箱在跑但没人认领」的残留——
// 这类残留只能靠人工清理，是运维债。
func (m *Manager) CreateSession(ctx context.Context, opts CreateSessionOptions) (store.Session, error) {
	var sess store.Session
	id := opts.ID
	if id == "" {
		id = newID()
	}

	spec := m.cfg.Agent
	if m.cfg.Creds != nil {
		// 真 key 不进沙箱：用短期虚拟 key 替换上游凭据，端点指向网关。
		env, expiresAt, err := m.cfg.Creds.IssueForSession(ctx, SessionScope{
			SessionID: id, TenantID: opts.TenantID, UserID: opts.UserID,
		})
		if err != nil {
			return sess, fmt.Errorf("签发沙箱凭据失败: %w", err)
		}
		spec.Env = mergeEnv(spec.Env, env)
		m.log.Printf("会话 %s 已签发沙箱凭据，有效期至 %s", id, expiresAt.Format(time.RFC3339))
	}

	// 出网代理：每个会话一个 nonce，凭据进 userinfo（客户端自动转成
	// Proxy-Authorization），代理据此把流量归到租户/会话。
	// 注意必须按会话生成，若把同一个 sandboxID 塞进共享的 cfg.Agent.Env，
	// 所有会话的流量会挤在一个归属下，审计就失去意义。
	if m.cfg.Egress != nil && m.cfg.Egress.Enabled() {
		spec.Env = mergeEnv(spec.Env, egress.EnvForWithBypass(
			m.cfg.Egress.Addr(), id, newNonce(), m.cfg.EgressBypass))

		// 双层出网边界：代理管「哪些域名能出去」，Seatbelt 管「只能连代理」。
		// EGRESS_NO_PROXY 里的本机地址也要放行，否则 LLM 网关会被一起挡掉。
		if m.cfg.NetworkLock {
			var extra []string
			for _, a := range strings.Split(m.cfg.EgressBypass, ",") {
				if a = strings.TrimSpace(a); a != "" {
					extra = append(extra, a)
				}
			}
			spec.NetworkPolicy = &sandbox.NetworkPolicy{
				Enabled:    true,
				ProxyAddr:  m.cfg.Egress.Addr(),
				ExtraAllow: extra,
			}
		}
	}

	// 第三方凭据：只注入该 租户+用户 自己已授权的连接器。
	// proxy 模式已实现：注入的是 sidecar 基址而非 token，token 由 egress sidecar
	// 转发时按用户取用注入；连接器是增强项，注入失败只记日志，不挡会话创建。
	if m.cfg.Connectors != nil && m.cfg.Connectors.Enabled() {
		if names := m.connectedNames(ctx, opts.TenantID, opts.UserID); len(names) > 0 {
			env, err := m.cfg.Connectors.Inject(ctx, opts.TenantID, opts.UserID, names)
			if err != nil {
				m.log.Printf("会话 %s 注入连接器凭据失败（不影响会话）: %v", id, err)
			} else {
				spec.Env = mergeEnv(spec.Env, env)
			}
		}
	}

	// 让沙箱里的 astra-cli 真能跑：地址 + 会话级受限凭据 + 二进制路径。
	// 凭据用 scope=sandbox 的短期 key，不是平台 JWT；API 侧有窄白名单与归属校验。
	if h := m.cfg.Hints; h != nil {
		_, url, cli := h.Snapshot()
		env := map[string]string{}
		if url != "" {
			env["ASTRA_SERVER"] = url
		}
		if cli != "" {
			env["ASTRA_CLI_BIN"] = cli
		}
		if m.cfg.SandboxKeyIssuer != nil && opts.TenantID != "" && opts.UserID != "" {
			if tok, _, err := m.cfg.SandboxKeyIssuer.IssueSandboxKey(
				opts.TenantID, opts.UserID, id, 0); err == nil {
				env["ASTRA_TOKEN"] = tok
			} else {
				m.log.Printf("会话 %s 签发沙箱凭据失败（skill 内 CLI 将不可用）: %v", id, err)
			}
		}
		spec.Env = mergeEnv(spec.Env, env)
		// 开了网络锁时，控制面回环地址必须显式放行，否则 CLI 连不上 7788。
		if m.cfg.NetworkLock && url != "" && spec.NetworkPolicy != nil {
			if hp := hostPortOf(url); hp != "" {
				spec.NetworkPolicy.ExtraAllow = append(spec.NetworkPolicy.ExtraAllow, hp)
			}
		}
	}

	req := sandbox.CreateRequest{
		ID:    id,
		Agent: spec,
		Token: opts.Token,
	}
	sb, err := m.cfg.Provider.Create(ctx, req)
	if err != nil {
		return sess, fmt.Errorf("起沙箱失败: %w", err)
	}
	// 登记放在 Create 成功之后：失败的沙箱不会留下孤立映射；
	// 后续握手失败走 cleanupSandbox 统一注销。runID 此刻还不存在，
	// StartRun 会用真正的 runID 覆盖。
	if m.cfg.Egress != nil && m.cfg.Egress.Enabled() {
		m.cfg.Egress.Register(id, opts.TenantID, opts.UserID, id, "")
	}

	info, ierr := sb.Info(ctx)
	cwd := opts.Cwd
	if cwd == "" {
		if ierr != nil {
			m.cleanupSandbox(sb)
			return sess, fmt.Errorf("读沙箱信息失败: %w", ierr)
		}
		cwd = info.Workspace
	}

	client, err := m.dial(ctx, sb)
	if err != nil {
		m.cleanupSandbox(sb)
		return sess, err
	}

	agentSID, err := client.NewSession(ctx, cwd, nil)
	if err != nil {
		_ = client.Conn().Close()
		m.cleanupSandbox(sb)
		return sess, fmt.Errorf("session/new 失败: %w", err)
	}

	now := time.Now().UTC()
	sess = store.Session{
		ID:              id,
		TenantID:        opts.TenantID,
		UserID:          opts.UserID,
		ParentSessionID: opts.ParentSessionID,
		SandboxID:       id,
		AgentSessionID:  agentSID,
		Cwd:             cwd,
		Status:          store.SessionActive,
		CreatedAt:       now,
		LastActiveAt:    now,
	}
	if err := m.cfg.Store.CreateSession(ctx, sess); err != nil {
		_ = client.Conn().Close()
		m.cleanupSandbox(sb)
		return store.Session{}, err
	}

	m.mu.Lock()
	m.binds[id] = &binding{m: m, sess: sess, sem: make(chan struct{}, 1), client: client}
	m.mu.Unlock()

	m.log.Printf("会话 %s 就绪 agentSession=%s cwd=%s provider=%s", id, agentSID, cwd, m.cfg.Provider.Name())
	return sess, nil
}

// GetSession 读会话元数据。
func (m *Manager) GetSession(ctx context.Context, id string) (store.Session, error) {
	return m.cfg.Store.GetSession(ctx, id)
}

// ListSessions 列出全部会话。
func (m *Manager) ListSessions(ctx context.Context) ([]store.Session, error) {
	return m.cfg.Store.ListSessions(ctx)
}

// DeleteSession 销毁沙箱并删数据。会话不存在时返回 ErrNotFound。
//
// 顺序有讲究：先把会话标成 deleted（挡住新 run），再取消在跑的 run 并等它们收尾，
// 最后才删目录。反过来做的后果实测过：RemoveAll 和事件写入撞在一起，
// 删目录报 "directory not empty"，接口返回 500，而沙箱已经销毁——
// 留下一个「删不掉的半死会话」。
func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	// 1. 先标记，避免新请求挤进来
	sess, err := m.cfg.Store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	sess.Status = store.SessionDeleted
	if err := m.cfg.Store.PutSession(ctx, sess); err != nil {
		return err
	}

	// 2. 取消未结束的 run
	runs, _ := m.cfg.Store.ListRuns(ctx, id)
	for _, r := range runs {
		if !store.IsTerminal(r.Status) {
			if err := m.Cancel(ctx, r.ID); err != nil {
				m.log.Printf("删除会话时取消 run %s: %v", r.ID, err)
			}
		}
	}

	// 3. 等它们写完终态（有界等待，最多 5s；卡住也要继续删，否则会话永远删不掉）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pending := 0
		runs, _ := m.cfg.Store.ListRuns(ctx, id)
		for _, r := range runs {
			if !store.IsTerminal(r.Status) {
				pending++
			}
		}
		if pending == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 4. 断连接、销毁沙箱
	m.mu.Lock()
	b := m.binds[id]
	delete(m.binds, id)
	m.mu.Unlock()

	if b != nil {
		b.mu.Lock()
		if b.client != nil {
			_ = b.client.Conn().Close()
			b.client = nil
		}
		b.mu.Unlock()
	}

	// Destroy 可能撞上还没退完的 acp-proxy（并行测试/高并发删除时偶发），
	// 有界重试几次。最终仍失败也**继续删数据**：第 3 步已经定了这个取舍——
	// 让「会话删不掉」变成 500、留下一个标着 deleted 却仍可查到的半死会话，
	// 比留一个待回收的沙箱更糟；沙箱由 lifecycle reaper 兜底回收。
	// 实测症状：TestSessionLifecycle 在全量并行下偶发「删会话返回 500 /
	// 删后查会话返回 200」。
	var derr error
	for i := 0; i < 8; i++ {
		derr = m.cfg.Provider.Destroy(ctx, id)
		if derr == nil || errors.Is(derr, sandbox.ErrNotFound) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m.cfg.Egress != nil {
		m.cfg.Egress.Unregister(id)
	}
	if derr != nil && !errors.Is(derr, sandbox.ErrNotFound) {
		m.log.Printf("删除会话 %s 时销毁沙箱失败（继续删数据，沙箱交由 reaper 回收）: %v", id, derr)
	}

	// 5. 删数据。仍可能撞上还没来得及退出的写入方，重试几次。
	var lastErr error
	for i := 0; i < 20; i++ {
		if lastErr = m.cfg.Store.DeleteSession(ctx, id); lastErr == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return lastErr
}

// StartRun 创建 run 并异步执行，立刻返回 runId 供客户端订阅事件。
func (m *Manager) StartRun(ctx context.Context, sessionID, input string) (store.Run, error) {
	return m.StartRunWithParent(ctx, sessionID, input, "")
}

// StartRunWithParent 与 StartRun 同语义，额外记录派生来源。
// 拆成两个方法而不是给 StartRun 加参数：现有调用点（API / CLI / 调度器）很多，
// 改签名会让它们全部跟着改，而 parentRunID 只在 collab 这一条链路上有意义。
func (m *Manager) StartRunWithParent(ctx context.Context, sessionID, input, parentRunID string) (store.Run, error) {
	if input == "" {
		return store.Run{}, errors.New("runner: 输入为空")
	}
	sess, err := m.cfg.Store.GetSession(ctx, sessionID)
	if err != nil {
		return store.Run{}, err
	}
	if sess.Status == store.SessionDeleted {
		return store.Run{}, fmt.Errorf("%w: 会话已删除", store.ErrConflict)
	}
	// 第一次发指令时给会话起个名字，用户改过之后（Title 非空）不再覆盖。
	// 放在这里而不是建会话时：建会话时还不知道用户要干什么，起出来的都是「新会话 3」。
	if sess.Title == "" {
		sess.Title = summarizeInput(input)
		if err := m.cfg.Store.PutSession(ctx, sess); err != nil {
			m.log.Printf("写会话标题失败 %s: %v", sess.ID, err)
		}
	}

	// 失败诊断回灌：把同会话最近一条诊断作为明确的上下文前缀，
	// 不改写用户输入的语义，也不自动重试/改参数（研究里自动改代码已有作弊风险）。
	if m.cfg.InsightReplay && m.cfg.Hints != nil {
		if ins, _, _ := m.cfg.Hints.Snapshot(); ins != nil {
			if ds, err := ins.Recent(ctx, sessionID, 1); err == nil && len(ds) > 0 {
				d := ds[0]
				input = "[上轮失败上下文] " + d.Error + "；建议：" + d.Suggestion +
					"\n\n[本轮指令]\n" + input
			}
		}
	}

	run := store.Run{
		ID:          newID(),
		SessionID:   sessionID,
		ParentRunID: parentRunID,
		Status:      store.RunQueued,
		Input:       input,
		CreatedAt:   time.Now().UTC(),
	}
	if err := m.cfg.Store.CreateRun(ctx, run); err != nil {
		return store.Run{}, err
	}
	// 出网审计带上 runID：同一会话并发多个 run 时后写的赢，
	// 但至少能把流量定位到「最近一轮」，比只有 sessionID 强。
	if m.cfg.Egress != nil && m.cfg.Egress.Enabled() && sess.SandboxID != "" {
		m.cfg.Egress.Register(sess.SandboxID, sess.TenantID, sess.UserID, sessionID, run.ID)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.execute(run)
	}()
	return run, nil
}

// Cancel 请求 agent 停止当前任务。真正的终态要等 prompt 返回 cancelled，
// 因为 agent 可能刚好在取消前就结束了。
func (m *Manager) Cancel(ctx context.Context, runID string) error {
	run, err := m.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if store.IsTerminal(run.Status) {
		return nil
	}

	// 先标记意图再通知 agent：agent 可能在收到通知的同一毫秒就把 prompt 返回掉，
	// 反过来写会漏掉「错误已返回、标记还没落」的窗口。
	// requestCancel 同时告诉我们 prompt 是否已经发出去——没发出去就别发了。
	dispatched := m.gate.requestCancel(runID)

	// 结掉待审批：agent 正卡在 request_permission 上等我们回复，
	// 只发 session/cancel 的话它要等这条请求被答复才会去读取消通知。
	m.releaseApprovals(runID, DecisionDeny)

	if !dispatched {
		// prompt 还在路上，execute 会在 beforePrompt 那里看到取消意图并自行收尾。
		return nil
	}

	m.mu.Lock()
	b := m.binds[run.SessionID]
	m.mu.Unlock()
	if b == nil {
		return fmt.Errorf("%w: 会话 %s 没有活跃绑定", store.ErrNotFound, run.SessionID)
	}

	b.mu.Lock()
	client := b.client
	b.mu.Unlock()
	if client == nil {
		return fmt.Errorf("%w: 会话没有 ACP 连接", store.ErrNotFound)
	}
	return client.Cancel(b.sess.AgentSessionID)
}

// RecoverInterrupted 把上一个进程留下的「未结束」run 归档。
//
// 必须有这一步：run 的执行体在内存里（goroutine + ACP 连接），进程一退就没了，
// 但状态还停在 running / waiting_approval。不清理的话前端会看到一个永远转圈的
// 任务，而审批注册表是空的——用户点「允许」只会得到 404。
//
// 为什么不「续跑」：一次 prompt 的执行上下文（agent 会话里的中间状态、已经批过
// 的权限）已经丢了，重放会得到半截结果。老实标成失败，让用户重发，比假装续上更可信。
func (m *Manager) RecoverInterrupted(ctx context.Context) (int, error) {
	// 先把悬挂的审批标成 expired。上一个进程里等它的人已经不在了，
	// 这条请求不可能再被任何人处理；留成 pending 会让界面和库都说谎。
	// 事件必须单独发：前端把卡片从「等你决定」改成「已失效」靠的是 approval_result，
	// 只看 run 状态的话，用户不知道那条审批到底怎么了。
	expired, err := m.apprStore.ExpirePendingApprovals(ctx, time.Now().UTC())
	if err != nil {
		// 过期失败不阻断 run 归档：run 的终态更重要，审批残留可以人工清。
		m.log.Printf("过期悬挂审批失败: %v", err)
	}
	for _, a := range expired {
		ev, err := m.cfg.Store.Append(ctx, a.RunID, store.NewEvent{
			Type: store.EventApprovalResult,
			Payload: map[string]any{
				"toolCallId": a.ToolCallID,
				"decision":   DecisionDeny,
				"by":         "system",
				"reason":     "expired",
			},
		})
		if err != nil {
			m.log.Printf("为失效审批 %s/%s 写 approval_result 失败: %v", a.RunID, a.ToolCallID, err)
			continue
		}
		if m.cfg.Hub != nil {
			m.cfg.Hub.Publish(ev)
		}
	}

	sessions, err := m.cfg.Store.ListSessions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		runs, err := m.cfg.Store.ListRuns(ctx, s.ID)
		if err != nil {
			return n, err
		}
		for _, r := range runs {
			if store.IsTerminal(r.Status) {
				continue
			}
			r.Status = store.RunFailed
			r.Error = "控制面重启，本轮中断"
			r.EndedAt = time.Now().UTC()
			if err := m.cfg.Store.UpdateRun(ctx, r); err != nil {
				m.log.Printf("归档中断的 run %s 失败: %v", r.ID, err)
				continue
			}
			ev, err := m.cfg.Store.Append(ctx, r.ID, store.NewEvent{
				Type: store.EventState,
				Payload: map[string]any{
					"status": store.RunFailed,
					"reason": "control_plane_restart",
					"error":  r.Error,
				},
			})
			if err == nil && m.cfg.Hub != nil {
				m.cfg.Hub.Publish(ev)
			}
			n++
		}
	}
	return n, nil
}

// Close 释放全部连接。**不销毁沙箱**——worker 重启后应该能重新接管。
func (m *Manager) Close() error {
	m.mu.Lock()
	binds := make([]*binding, 0, len(m.binds))
	for _, b := range m.binds {
		binds = append(binds, b)
	}
	m.binds = map[string]*binding{}
	m.mu.Unlock()

	for _, b := range binds {
		b.mu.Lock()
		if b.client != nil {
			_ = b.client.Conn().Close()
			b.client = nil
		}
		b.mu.Unlock()
	}

	// 控制面退出：先把所有还挂着的审批按拒绝结掉，再等 goroutine 收工。
	// 顺序反了会死锁——等待审批的 run 不放行就永远不会退出。
	m.apprMu.Lock()
	runIDs := make([]string, 0, len(m.apprs))
	for _, w := range m.apprs {
		runIDs = append(runIDs, w.RunID)
	}
	m.apprMu.Unlock()
	for _, id := range runIDs {
		m.releaseApprovals(id, DecisionDeny)
	}

	m.cancel()
	m.wg.Wait()
	return nil
}

// ── 执行 ─────────────────────────────────────────────────────

func (m *Manager) execute(run store.Run) {
	ctx := m.baseCtx
	s := newSink(ctx, m.cfg.Store, m.cfg.Hub, run.ID, m.cfg.CoalesceWindow)
	defer func() { _ = s.close() }()

	b, err := m.bindingFor(run.SessionID)
	if err != nil {
		m.finish(ctx, run, s, store.RunFailed, "", err)
		return
	}

	// 同一会话串行：第二个 prompt 必须等第一个结束，否则 agent 侧会拒绝
	// （ACP 不变量：一个会话同时只能有一个进行中的 prompt）。
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		m.finish(ctx, run, s, store.RunCancelled, "", ctx.Err())
		return
	}
	defer func() { <-b.sem }()

	b.mu.Lock()
	client, err := m.ensureClient(ctx, b)
	b.mu.Unlock()
	if err != nil {
		m.finish(ctx, run, s, store.RunFailed, "", err)
		return
	}

	run.Status = store.RunRunning
	run.StartedAt = time.Now().UTC()
	_ = m.cfg.Store.UpdateRun(ctx, run)
	s.event(store.EventState, map[string]any{"status": store.RunRunning})

	// run_state 一推出去，前端就可能点停止。这时候 prompt 还没发，
	// 发过去也没人接（见 cancel.go），所以直接收工。
	if !m.gate.beforePrompt(run.ID) {
		m.gate.forget(run.ID)
		m.finish(ctx, run, s, store.RunCancelled, "cancelled", nil)
		return
	}

	// 回调挂在共享 client 上。因为 sem 保证了同一会话只有一个 run 在跑，
	// 这里覆盖回调是安全的。
	client.OnUpdate = func(u acp.SessionUpdate) { m.dispatch(s, u) }
	client.OnPermission = func(_ context.Context, p acp.RequestPermissionParams) (acp.PermissionOutcome, error) {
		return m.decide(s, run.ID, p)
	}

	res, err := client.Prompt(ctx, b.sess.AgentSessionID, acp.Text(run.Input))
	cancelled := m.gate.afterPrompt(run.ID)
	m.gate.forget(run.ID)
	switch {
	case cancelled:
		// 用户按过停止：不管 agent 是回 cancelled、报错（可能正卡在权限请求上被掐断）
		// 还是恰好跑完了，统一记成取消。「停止」按钮的语义要可预测。
		m.finish(ctx, run, s, store.RunCancelled, "cancelled", nil)
		return
	case err != nil:
		m.finish(ctx, run, s, store.RunFailed, "", fmt.Errorf("prompt 失败: %w", err))
		return
	}

	status := stopReasonToStatus(res.StopReason)
	m.finish(ctx, run, s, status, res.StopReason, nil)

	// 会话活跃时间
	if sess, err := m.cfg.Store.GetSession(ctx, run.SessionID); err == nil {
		sess.LastActiveAt = time.Now().UTC()
		_ = m.cfg.Store.PutSession(ctx, sess)
	}
}

func (m *Manager) finish(ctx context.Context, run store.Run, s *sink, status, stopReason string, cause error) {
	// 保险：正常路径下审批请求必然先结束，run 才会走到这里。
	// 但 panic / 连接断开时可能留下孤儿，不结掉的话等待方会一直挂着。
	m.releaseApprovals(run.ID, DecisionDeny)

	// 顺序很重要，而且不能只写「先事件还是先元数据」——两类客户端方向相反：
	//   - 轮询 run.Status 的（前端首屏 / CLI / history）看到终态就拉事件流，
	//     终态事件必须已经落库，否则最后一条是 message_delta；
	//   - 订阅 SSE 的收到「结束」立刻查 run，run 必须已是终态，否则自相矛盾。
	// 所以把终态事件拆成「先落库、后发布」两步，中间夹着元数据写入。
	s.flush()
	if cause != nil {
		s.event(store.EventError, map[string]any{"message": cause.Error()})
	}
	terminal := s.stage(store.EventState, map[string]any{"status": status, "stopReason": stopReason})

	run.Status = status
	run.StopReason = stopReason
	run.EndedAt = time.Now().UTC()
	if cause != nil {
		run.Error = cause.Error()
	}
	if err := m.cfg.Store.UpdateRun(context.WithoutCancel(ctx), run); err != nil && !isGone(err) {
		m.log.Printf("run %s 终态写入失败: %v", run.ID, err)
	}
	// 元数据已经是终态了，现在才让订阅者看到「结束」。
	s.publish(terminal)
	if cause != nil {
		m.log.Printf("run %s 失败: %v", run.ID, cause)
	} else {
		m.log.Printf("run %s 结束 status=%s stopReason=%s", run.ID, status, stopReason)
	}

	// 指标埋在这里而不是 API 层：run 也可能因超时、重连、进程退出而结束，
	// 那些路径根本不经过 HTTP handler，挂在 API 上会漏掉最需要看到的失败。
	if !run.StartedAt.IsZero() {
		obs.RecordRunFinished(status, run.EndedAt.Sub(run.StartedAt))
	}
}

func (m *Manager) bindingFor(sessionID string) (*binding, error) {
	m.mu.Lock()
	b := m.binds[sessionID]
	m.mu.Unlock()
	if b != nil {
		return b, nil
	}

	// 进程重启过：从 store 恢复绑定，沙箱靠 provider.Acquire 接回来。
	sess, err := m.cfg.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		return nil, err
	}
	b = &binding{m: m, sess: sess, sem: make(chan struct{}, 1)}

	m.mu.Lock()
	if exist, ok := m.binds[sessionID]; ok {
		m.mu.Unlock()
		return exist, nil
	}
	m.binds[sessionID] = b
	m.mu.Unlock()
	return b, nil
}

// ensureClient 保证有一条可用的 ACP 连接。断开时重连，沙箱停了就重新拉起。
func (m *Manager) ensureClient(ctx context.Context, b *binding) (*acp.Client, error) {
	if b.client != nil {
		select {
		case <-b.client.Conn().Done():
			_ = b.client.Conn().Close()
			b.client = nil
		default:
			return b.client, nil
		}
	}

	sb, err := m.cfg.Provider.Acquire(ctx, b.sess.ID)
	if errors.Is(err, sandbox.ErrNotRunning) || errors.Is(err, sandbox.ErrNotFound) {
		// local 的 Create 对同 ID 是幂等的：沿用上次的端口与 agent 命令，只重启进程。
		// 不传 Token：provider 会沿用 meta 里已有凭据，
		// 控制面因此不需要持有明文 token。
		sb, err = m.cfg.Provider.Create(ctx, sandbox.CreateRequest{
			ID:    b.sess.ID,
			Agent: m.cfg.Agent,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("取沙箱失败: %w", err)
	}

	client, err := m.dial(ctx, sb)
	if err != nil {
		return nil, err
	}

	// 重连意味着我们这边断过一次（控制面重启 / 网络抖动）。此刻 agent 里可能
	// 还挂着一条 prompt——最常见的是卡在 request_permission 上等一个永远不会来
	// 的回复（提问的人已经随进程一起没了）。先发 cancel 把它清掉，再 resume：
	// 否则紧接着的 prompt 会被 agent 以「会话已有进行中的命令」拒掉，
	// 用户看到的是「重启之后这个会话就废了」。
	// 顺序反了（先 resume）在 dsh 上会返回 already active，掩盖真正的问题。
	_ = client.Cancel(b.sess.AgentSessionID)

	// 恢复 ACP 会话。两种情况语义不同（M0 实测）：
	//   agent 还活着（只是我们断了 WS）→ 会话在内存里，resume 会报 already active
	//   agent 重启过（沙箱重建）    → 从持久化目录恢复，必须 resume 才能继续
	// 所以这里不能把 resume 失败当致命错误，交给随后的 prompt 去暴露真正的问题。
	if err := client.Resume(ctx, b.sess.AgentSessionID, b.sess.Cwd); err != nil {
		m.log.Printf("会话 %s resume 未成功（agent 可能仍持有会话）: %v", b.sess.ID, err)
	}

	b.client = client
	return client, nil
}

func (m *Manager) dial(ctx context.Context, sb sandbox.Sandbox) (*acp.Client, error) {
	ep := sb.Endpoint()
	ws, err := acp.DialWS(ctx, ep.URL, ep.Token)
	if err != nil {
		return nil, fmt.Errorf("连 acp-proxy 失败: %w", err)
	}
	client := acp.NewClient(acp.NewConn(ws))

	initCtx, cancel := context.WithTimeout(ctx, m.cfg.ConnectTimeout)
	defer cancel()
	if _, err := client.Initialize(initCtx, acp.ClientCapabilities{
		FS:       &acp.FSCapabilities{ReadTextFile: true, WriteTextFile: true},
		Terminal: true,
	}); err != nil {
		_ = client.Conn().Close()
		return nil, fmt.Errorf("ACP 握手失败: %w", err)
	}
	return client, nil
}

// newNonce 生成每条沙箱独立的代理凭据片段。
func newNonce() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// connectedNames 列出该用户已授权的连接器名，交给 Inject 决定注入哪些 env。
// 读失败只记日志：没连上连接器不该影响会话创建。
func (m *Manager) connectedNames(ctx context.Context, tenantID, userID string) []string {
	creds, err := m.cfg.Connectors.ListCredentials(ctx, tenantID, userID)
	if err != nil {
		m.log.Printf("读连接器凭据列表失败（不影响会话）: %v", err)
		return nil
	}
	out := make([]string, 0, len(creds))
	for _, c := range creds {
		out = append(out, c.Name)
	}
	return out
}

// mergeEnv 把 over 合并进 base 的副本，同名以 over 为准。
//
// 必须返回副本：AgentSpec 是 Manager 级别的共享值，就地改会让下一个会话
// 继承上一个会话的凭据——那不是配置问题，是越权事故。
func mergeEnv(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func (m *Manager) cleanupSandbox(sb sandbox.Sandbox) {
	_ = sb.Close()
	if m.cfg.Egress != nil {
		m.cfg.Egress.Unregister(sb.ID())
	}
	if err := m.cfg.Provider.Destroy(context.WithoutCancel(m.baseCtx), sb.ID()); err != nil &&
		!errors.Is(err, sandbox.ErrNotFound) {
		m.log.Printf("清理沙箱 %s 失败: %v", sb.ID(), err)
	}
}

// ── 事件映射 ─────────────────────────────────────────────────

// dispatch 把一条 session/update 转成事件。
//
// 未知类型不丢弃，落成 agent_log——上游新增一种 update 时，
// 我们要能在事件流里看到它，而不是静默消失只能靠抓包猜。
func (m *Manager) dispatch(s *sink, u acp.SessionUpdate) {
	var kind acp.UpdateKind
	if err := json.Unmarshal(u.Update, &kind); err != nil {
		s.event(store.EventAgentLog, map[string]any{"error": "无法解析 update", "raw": string(u.Update)})
		return
	}

	switch kind.Type {
	case "agent_message_chunk":
		var c acp.AgentMessageChunk
		if err := json.Unmarshal(u.Update, &c); err == nil {
			s.delta(store.EventMessageDelta, c.Content.Text)
		} else {
			s.event(store.EventAgentLog, map[string]any{"error": err.Error(), "raw": string(u.Update)})
		}
	case "agent_thought_chunk":
		var c acp.AgentMessageChunk
		if err := json.Unmarshal(u.Update, &c); err == nil {
			s.delta(store.EventThoughtDelta, c.Content.Text)
		}
	case "tool_call":
		var c acp.ToolCall
		if err := json.Unmarshal(u.Update, &c); err == nil {
			s.event(store.EventToolStart, map[string]any{
				"toolCallId": c.ToolCallID,
				"title":      c.Title,
				"kind":       c.Kind,
				"status":     c.Status,
				"rawInput":   rawOrNil(c.RawInput),
				"rawOutput":  rawOrNil(c.RawOutput),
				"content":    rawOrNil(c.Content),
			})
		}
	case "tool_call_update":
		var c acp.ToolCallUpdate
		if err := json.Unmarshal(u.Update, &c); err == nil {
			s.event(store.EventToolUpdate, map[string]any{
				"toolCallId": c.ToolCallID,
				"status":     c.Status,
				"title":      c.Title,
				"kind":       c.Kind,
				"rawOutput":  rawOrNil(c.RawOutput),
				"content":    rawOrNil(c.Content),
			})
		}
	case "user_message_chunk", "plan", "available_commands_update", "current_mode_update":
		// 已知但当前不落库：这些要么是回显，要么是 M2 才用到的会话元信息。
		s.event(store.EventAgentLog, map[string]any{"kind": kind.Type})
	default:
		s.event(store.EventAgentLog, map[string]any{"kind": kind.Type, "raw": string(u.Update)})
	}
}

func (m *Manager) decide(s *sink, runID string, p acp.RequestPermissionParams) (acp.PermissionOutcome, error) {
	kinds := make([]string, 0, len(p.Options))
	for _, o := range p.Options {
		kinds = append(kinds, o.Kind)
	}
	base := map[string]any{
		"toolCallId": p.ToolCall.ToolCallID,
		"title":      p.ToolCall.Title,
		"kind":       p.ToolCall.Kind,
		"options":    kinds,
		"policy":     m.cfg.Policy.Name,
	}

	// 交互模式：白名单外的操作挂起等人。**这里会阻塞**，阻塞的是 ACP 请求
	// 的 goroutine，不是事件流——所以前端能一边看到「等待审批」，一边照常刷新。
	if m.cfg.Policy.Interactive && !m.cfg.Policy.AutoAllowed(p.ToolCall.Kind) {
		return m.awaitHuman(s, runID, p, base)
	}

	opt, allowed := m.cfg.Policy.Decide(p)
	base["decision"] = opt.Kind
	base["allowed"] = allowed
	base["pending"] = false
	s.event(store.EventApprovalReq, base)

	if opt.OptionID == "" {
		return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
	}
	return acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: opt.OptionID}, nil
}

// awaitHuman 把一次权限请求挂到审批面板上，一直等到有人点「允许 / 拒绝」。
//
// 三种结束方式，都会落一条 approval_result：
//   - 人工决策
//   - 超时（按拒绝处理，并把 timeout 记进事件，便于事后区分「没人管」和「明确拒绝」）
//   - run 被取消 / 控制面退出（按拒绝处理）
func (m *Manager) awaitHuman(s *sink, runID string, p acp.RequestPermissionParams, base map[string]any) (acp.PermissionOutcome, error) {
	now := time.Now().UTC()
	pending := PendingApproval{
		RunID:      runID,
		ToolCallID: p.ToolCall.ToolCallID,
		Title:      p.ToolCall.Title,
		Kind:       p.ToolCall.Kind,
		CreatedAt:  now,
		ExpiresAt:  now.Add(m.cfg.ApprovalTimeout),
	}

	// run 状态先切成 waiting_approval：随后写库的 pending 记录一旦被查到，
	// run 状态必须已经是「在等人」，否则前端拉到审批却看到 run 还在 running。
	// 排障时也能一眼看出是谁在等。写失败不影响主流程。
	// 状态事件必须显式推一条：光改 store 的话，已经连着的事件流看不到变化，
	// 界面会一直显示「执行中」——用户就不知道是卡住了还是在等他点。
	m.setRunStatus(runID, store.RunWaitingApproval)
	s.event(store.EventState, map[string]any{"status": store.RunWaitingApproval})

	// 再落库：库是跨进程的唯一真相，本地 map 只是快路径。
	// 写失败不改变「继续等」的行为——总比把 agent 直接拒掉、让用户以为
	// 是权限策略生效更好；但这条审批在别的副本上查不到，日志必须留痕。
	if err := m.apprStore.CreateApproval(context.WithoutCancel(s.ctx), store.Approval{
		RunID:      pending.RunID,
		ToolCallID: pending.ToolCallID,
		Title:      pending.Title,
		Kind:       pending.Kind,
		Status:     store.ApprovalPending,
		CreatedAt:  pending.CreatedAt,
		ExpiresAt:  pending.ExpiresAt,
	}); err != nil {
		m.log.Printf("审批 %s/%s 落库失败（只有本进程能处理本次审批）: %v",
			runID, pending.ToolCallID, err)
	}

	w := m.registerApproval(pending)
	defer m.unregisterApproval(runID, p.ToolCall.ToolCallID)

	base["decision"] = "pending"
	base["allowed"] = false
	base["pending"] = true
	base["expiresAt"] = pending.ExpiresAt
	s.event(store.EventApprovalReq, base)

	timer := time.NewTimer(m.cfg.ApprovalTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()

	var decision, by string
wait:
	for {
		select {
		case decision = <-w.ch:
			by = "user"
			break wait
		case <-ticker.C:
			// 跨副本 / 跨进程：另一个副本改了库，本进程的 channel 永远不会响。
			a, err := m.apprStore.GetApproval(context.Background(), runID, p.ToolCall.ToolCallID)
			if err != nil || !store.ApprovalResolved(a.Status) {
				continue
			}
			decision = decisionOf(a.Status)
			by = a.ResolvedBy
			if by == "" {
				by = "system"
			}
			break wait
		case <-timer.C:
			decision, by = DecisionTimeout, "system"
			break wait
		case <-m.baseCtx.Done():
			decision, by = DecisionDeny, "system"
			break wait
		}
	}

	// 本地决定可能输给库里的另一个写入方（用户点击 vs 超时 / 取消）。
	// 以库里的结论为准，否则事件里的 decision 会和审计记录相反。
	decision, by = m.settleApproval(runID, p.ToolCall.ToolCallID,
		approvalStatusOf(decision), by, decision, by)

	m.log.Printf("审批 %s/%s → %s（%s）", runID, p.ToolCall.ToolCallID, decision, by)
	// 三种结论都记：allow 高不代表健康，deny/timeout 比例才是「权限配得太紧」
	// 还是「没人看审批」的信号。这里的 by 区分了人点拒绝与系统代拒。
	obs.RecordApproval(decision)
	s.event(store.EventApprovalResult, map[string]any{
		"toolCallId": p.ToolCall.ToolCallID,
		"decision":   decision,
		"by":         by,
	})
	// 有取消意图时不要置回 running：收尾会立刻写 cancelled，
	// 中间这次写入除了制造竞态没有别的作用（见 store 侧「终态不可逆」的注释）。
	if !m.gate.cancelled(runID) {
		m.setRunStatus(runID, store.RunRunning)
		s.event(store.EventState, map[string]any{"status": store.RunRunning})
	}

	if decision == DecisionAllow {
		if opt, ok := p.Select(acp.KindAllowOnce); ok {
			return acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: opt.OptionID}, nil
		}
	}
	if opt, ok := p.Select(acp.KindRejectOnce); ok {
		return acp.PermissionOutcome{Outcome: acp.OutcomeSelected, OptionID: opt.OptionID}, nil
	}
	// agent 没给任何可用选项：只能按协议回 cancelled，让它自己收尾。
	return acp.PermissionOutcome{Outcome: acp.OutcomeCancelled}, nil
}

// setRunStatus 只改 run 的状态字段，不碰其它元数据。
// 读-改-写之间没有锁，但同一 run 的元数据只由它自己的执行协程写，所以安全。
func (m *Manager) setRunStatus(runID, status string) {
	r, err := m.cfg.Store.GetRun(m.baseCtx, runID)
	if err != nil {
		return
	}
	if r.Status == status || store.IsTerminal(r.Status) {
		return
	}
	r.Status = status
	if err := m.cfg.Store.UpdateRun(m.baseCtx, r); err != nil && !isGone(err) {
		m.log.Printf("更新 run %s 状态失败: %v", runID, err)
	}
}

// isGone 判断错误是不是「目标已经不在」。
//
// 用在删除会话的收尾阶段：run 的 goroutine 可能在目录被删之后再写一次终态，
// 那不是故障，只是输掉了一场竞态。把它记成错误只会让人以为删除有问题。
func isGone(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// summarizeInput 从用户输入里提炼一个会话标题。
//
// 只取首行、并在第一个 shell 连接符处截断：本地开发默认用 shell agent，
// 输入就是一条命令，整条塞进标题会出现「mkdir -p reports && printf '# 周报…」这种
// 侧栏放不下、也读不出意图的字符串。自然语言输入里这些符号基本不出现，截断无副作用。
// 按字符（不是字节）截断——中文按 byte 截会切出乱码。
func summarizeInput(input string) string {
	s := strings.TrimSpace(input)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	for _, sep := range []string{" && ", " || ", " | ", " ; ", "; "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 28 {
		return string(r[:28]) + "…"
	}
	return s
}

func stopReasonToStatus(stopReason string) string {
	switch stopReason {
	case "cancelled":
		return store.RunCancelled
	case "end_turn", "max_tokens", "max_turn_requests", "refusal":
		return store.RunCompleted
	default:
		return store.RunCompleted
	}
}

func rawOrNil(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return json.RawMessage(r)
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// hostPortOf 从 "http://127.0.0.1:7788" 取出 "127.0.0.1:7788"，
// 供 Seatbelt 的 ExtraAllow 使用（SBPL 只认数值/ localhost + 端口）。
func hostPortOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
