package memory

// 自动入库：run 结束时把「用户输入 + 最终回复」摘要写进长期记忆。
//
// 为什么要摘要而不是逐条存：token 级增量信噪比极低（一次长回答几千条），
// 且会把向量库撑爆。只有「这一轮问了什么、最后答了什么」对下一次检索有意义。
//
// 归属（tenant/user）与输入**不在事件里**：事件只有 runID 和状态，所以必须
// 反查 run → session。这也是唯一能保证「不写错人」的路径——绝不能让写入方
// 自己声明 tenant/user。

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// errNoOwner 表示会话没有 tenant/user。写进去就是「谁都能查」的记忆，宁可不写。
var errNoOwner = errors.New("会话没有归属（单租户兼容模式），不写无主记忆")

const (
	// autoTextLimit 单侧（用户输入 / 助手回复）保留的字数上限。
	autoTextLimit = 2000
	// replyBuffer 单个 run 的回复缓冲上限（字节）。只用来做摘要，
	// 没必要把整篇长回答留在内存里等 run 结束。
	replyBuffer = 8 << 10
	// seenLimit 幂等表上限。按 FIFO 淘汰，1 万条约等于几十万个 run 之后才会失效，
	// 而同一个 runID 的重复终态事件必然发生在分钟级内。
	seenLimit = 10000
	// pendingLimit 同时处于「运行中」的 run 上限，防止异常情况下无界增长。
	pendingLimit = 4096
	// autoWriteTimeout 写一条记忆的超时。自动入库是旁路，不能拖住任何东西。
	autoWriteTimeout = 20 * time.Second
)

// RunInfo 是写记忆所需的 run 上下文。
type RunInfo struct {
	TenantID  string
	UserID    string
	SessionID string
	Input     string
}

// Runs 反查 run 的归属与输入。store.Store 天然满足这个接口，
// 所以装配时直接把 store.Store 传进来即可，不需要写适配器。
type Runs interface {
	GetRun(ctx context.Context, id string) (store.Run, error)
	GetSession(ctx context.Context, id string) (store.Session, error)
}

// Attach 订阅 hub，在 run 结束把这一轮的输入/输出摘要写进记忆。
//
// 注意：事件里没有 tenant/user/input，本函数无法解析归属，因此**不会写入**，
// 只会打一条警告。真正可用的是 AttachWith(store, hub, runs, logger)，
// 传进来的 runs 就是 app 层的 store.Store。保留这个签名是为了不改变既有契约。
func Attach(st Store, hub *eventbus.Hub, lg *log.Logger) (stop func()) {
	return AttachWith(st, hub, nil, lg)
}

// AttachWith 是 Attach 的完整版本：runs 用于把 runID 反查成归属与输入。
func AttachWith(st Store, hub *eventbus.Hub, runs Runs, lg *log.Logger) (stop func()) {
	if lg == nil {
		lg = log.Default()
	}
	// 能力关着的时候不启动任何 goroutine，也不订阅 hub：
	// 少一个后台协程，就少一处「关了还在偷偷占资源」的意外。
	if st == nil || IsDisabled(st) {
		return func() {}
	}
	if hub == nil {
		lg.Printf("memory: hub 为空，run 结束自动入库未启动")
		return func() {}
	}
	if runs == nil {
		lg.Printf("memory: 自动入库缺少 run 归属查询（请改用 AttachWith(store, hub, runs, logger)），未启动")
		return func() {}
	}

	a := &attacher{
		st:      st,
		runs:    runs,
		lg:      lg,
		pending: make(map[string]*pendingRun),
		seen:    make(map[string]time.Time),
	}
	// Hub 只支持一个全局消费者（后注册的覆盖先注册的），所以装配时必须只注册一次。
	hub.OnGlobal(a.onEvent)
	return a.stop
}

type pendingRun struct {
	reply strings.Builder
}

type attacher struct {
	st   Store
	runs Runs
	lg   *log.Logger

	mu      sync.Mutex
	pending map[string]*pendingRun
	// seen 是幂等表：同一个 runID 只写一次。
	seen  map[string]time.Time
	order []string
	// writeMu 把并发写串行化，避免同一时刻对 Milvus 发起大量 insert
	// （自动入库是旁路，宁可排队也不要和在线检索抢连接）。
	writeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
}

func (a *attacher) stop() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.pending = map[string]*pendingRun{}
	a.mu.Unlock()
	a.wg.Wait()
}

// onEvent 在 Publish 的 goroutine 里同步执行：必须快，不能阻塞，不能 panic。
// 所有 I/O 都甩到独立 goroutine 里做。
func (a *attacher) onEvent(ev store.Event) {
	if ev.RunID == "" || ev.Type == "" {
		return
	}

	var runID, status, reply string
	var launch bool

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	switch ev.Type {
	case store.EventMessageDelta:
		p := a.pending[ev.RunID]
		if p == nil {
			if len(a.pending) >= pendingLimit {
				// 超出上限说明有大量 run 从未结束。丢掉新 run 的缓冲，
				// 而不是让 map 无限长——记忆是增强项，不能吃掉进程内存。
				a.mu.Unlock()
				return
			}
			p = &pendingRun{}
			a.pending[ev.RunID] = p
		}
		if p.reply.Len() < replyBuffer {
			p.reply.WriteString(payloadText(ev.Payload))
		}
	case store.EventState:
		st := payloadStatus(ev.Payload)
		if !autoIngestStatus(st) {
			a.mu.Unlock()
			return
		}
		p := a.pending[ev.RunID]
		delete(a.pending, ev.RunID)
		if p != nil {
			reply = p.reply.String()
		}
		if !a.markSeenLocked(ev.RunID) {
			// 同一个 run 的终态事件重复到达（重连补事件、重放），跳过。
			a.mu.Unlock()
			return
		}
		runID, status, launch = ev.RunID, st, true
		// 在锁内 Add：否则 stop() 可能刚好在 Unlock 与 Add 之间开始 Wait，
		// 导致它以为没有在飞的写入而提前返回。
		a.wg.Add(1)
	default:
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	if launch {
		go func() {
			defer a.wg.Done()
			// 自动入库是旁路：这里 panic 会带走整个控制面。
			// recover 后记一条日志，比让主循环崩掉划算得多。
			defer func() {
				if v := recover(); v != nil {
					a.lg.Printf("memory: 自动入库 run %s 发生 panic: %v", runID, v)
				}
			}()
			a.write(runID, status, reply)
		}()
	}
}

// write 解析归属并落库。失败就把幂等标记撤掉，允许后续重试。
func (a *attacher) write(runID, status, reply string) {
	ctx, cancel := context.WithTimeout(context.Background(), autoWriteTimeout)
	defer cancel()

	info, err := a.resolve(ctx, runID)
	if err != nil {
		a.lg.Printf("memory: 自动入库跳过 run %s: %v", runID, err)
		a.unmark(runID)
		return
	}
	input := clip(strings.TrimSpace(info.Input), autoTextLimit)
	reply = clip(strings.TrimSpace(reply), autoTextLimit)
	if input == "" && reply == "" {
		a.lg.Printf("memory: run %s 没有可入库的内容（未拿到输入与回复），跳过", runID)
		a.unmark(runID)
		return
	}

	a.writeMu.Lock()
	_, err = a.st.Ingest(ctx, Item{
		TenantID:  info.TenantID,
		UserID:    info.UserID,
		SessionID: info.SessionID,
		Kind:      KindConversation,
		Text:      "用户: " + input + "\n助手: " + reply,
		Meta: map[string]any{
			"run_id": runID,
			"status": status,
		},
	})
	a.writeMu.Unlock()

	if err != nil {
		a.lg.Printf("memory: 自动入库 run %s 失败: %v", runID, err)
		a.unmark(runID)
	}
}

func (a *attacher) resolve(ctx context.Context, runID string) (RunInfo, error) {
	run, err := a.runs.GetRun(ctx, runID)
	if err != nil {
		return RunInfo{}, err
	}
	sess, err := a.runs.GetSession(ctx, run.SessionID)
	if err != nil {
		return RunInfo{}, err
	}
	if sess.TenantID == "" || sess.UserID == "" {
		// 单租户兼容模式下会话可能没有归属。写进去就是「谁都能查」的记忆，
		// 宁可不写。
		return RunInfo{}, errNoOwner
	}
	return RunInfo{
		TenantID:  sess.TenantID,
		UserID:    sess.UserID,
		SessionID: sess.ID,
		Input:     run.Input,
	}, nil
}

func (a *attacher) markSeenLocked(runID string) bool {
	if _, ok := a.seen[runID]; ok {
		return false
	}
	a.seen[runID] = time.Now()
	a.order = append(a.order, runID)
	if len(a.order) > seenLimit {
		drop := len(a.order) - seenLimit
		for _, k := range a.order[:drop] {
			delete(a.seen, k)
		}
		a.order = a.order[drop:]
	}
	return true
}

func (a *attacher) unmark(runID string) {
	a.mu.Lock()
	delete(a.seen, runID)
	a.mu.Unlock()
}

// autoIngestStatus 只记「跑完」和「取消」两种终态。
// failed 不记：失败的那一轮往往没有最终回复，写进去只是污染检索结果。
func autoIngestStatus(status string) bool {
	return status == store.RunCompleted || status == store.RunCancelled
}

func payloadStatus(raw json.RawMessage) string {
	var p struct {
		Status string `json:"status"`
	}
	if len(raw) == 0 {
		return ""
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Status
}

func payloadText(raw json.RawMessage) string {
	var p struct {
		Text string `json:"text"`
	}
	if len(raw) == 0 {
		return ""
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Text
}

// clip 按 rune 截断，避免把中文切成半个字。
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
