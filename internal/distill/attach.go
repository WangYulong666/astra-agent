package distill

// 自动沉淀：订阅全局事件，在 run 终态把这次轨迹变成一份草稿。
//
// 人仍然握着 accept 闸门（见 docs/design/self-evolving-loop.md §2.3）：
// 这里只负责「别让人记得手工点 POST /v1/runs/{id}/distill」。
//
// 为什么在 Attach 里读 DISTILL_AUTO：装配层不该为每个旁路能力加一个配置
// 字段再传进来，开关是运维语义，环境变量足够且只在进程启动时读一次。

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

const (
	// seenLimit 幂等表上限，FIFO 淘汰。1 万条够覆盖「同一个 run 的重复终态
	// 必然发生在分钟级内」这个前提，又不会让表无限长。
	seenLimit = 10000
	// autoDistillTimeout 是单次沉淀的超时。自动沉淀是旁路，不能无限期拖住
	// stop()，也不能和在线请求抢资源。
	autoDistillTimeout = 30 * time.Second
)

// Attach 订阅 hub，在 run 终态自动生成草稿。返回 stop：等所有在飞的沉淀
// goroutine 退出，调用方（装配层）在关停时调用，不许泄漏。
//
// DISTILL_AUTO=false 时直接返回 noop：不订阅、不起 goroutine。
func Attach(hub *eventbus.Hub, d Distiller, st store.Store, lg *log.Logger) func() {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DISTILL_AUTO")), "false") {
		return func() {}
	}
	if lg == nil {
		lg = log.Default()
	}
	if hub == nil || d == nil || st == nil || !d.Enabled() {
		// 关掉的沉淀器不该对每条终态刷一条 ErrDisabled 日志。
		return func() {}
	}
	a := &attacher{
		d:    d,
		st:   st,
		lg:   lg,
		seen: make(map[string]bool),
	}
	hub.OnGlobal(a.onEvent)
	return a.stop
}

type attacher struct {
	d  Distiller
	st store.Store
	lg *log.Logger

	mu     sync.Mutex
	seen   map[string]bool
	order  []string
	closed bool
	wg     sync.WaitGroup
}

// stop 设置 closed 后等所有在飞 goroutine 退出。可重复调用：WaitGroup.Wait
// 本身可重入，这里也不提前 return，避免第二次调用误以为已经停干净。
func (a *attacher) stop() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.wg.Wait()
}

// onEvent 在 Publish 的调用方 goroutine 里同步执行：必须快、不阻塞、不 panic。
// 只做终态判断 + 去重，其余（读事件流、写 drafts.json）全丢进 goroutine。
func (a *attacher) onEvent(ev store.Event) {
	if ev.Type != store.EventState || ev.RunID == "" {
		return
	}
	if !store.IsTerminal(payloadStatus(ev.Payload)) {
		return
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if !a.markSeenLocked(ev.RunID) {
		a.mu.Unlock()
		return // 同一个 run 只沉淀一次（重连补事件、重放都会重复推终态）
	}
	// 在锁内 Add：否则 stop() 可能刚好在 Unlock 与 Add 之间开始 Wait，
	// 以为没有在飞的任务而提前返回。
	a.wg.Add(1)
	a.mu.Unlock()

	go func(runID string) {
		defer a.wg.Done()
		// OnGlobal 外层有 recover，但这里再兜一层：panic 只该留一条日志，
		// 绝不能带走控制面，也不能让 stop() 永远等不到 Done。
		defer func() {
			if v := recover(); v != nil {
				a.lg.Printf("distill: 自动沉淀 run %s 发生 panic: %v", runID, v)
			}
		}()
		a.distill(runID)
	}(ev.RunID)
}

// distill 是 goroutine 里的重活：先按与 Distill 相同的口径数成功步数，
// 少于 2 步直接静默跳过（否则每轮纯问答都会刷一条 ErrTooFewSteps 日志）。
func (a *attacher) distill(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), autoDistillTimeout)
	defer cancel()

	events, err := a.st.Events(ctx, runID, 0, 0)
	if err != nil {
		a.lg.Printf("distill: 自动沉淀读 run %s 事件失败: %v", runID, err)
		return
	}
	// maxSteps=0 表示不截断：这里数的是「总共有几次成功工具调用」，
	// 不能用产出正文时的截断步数，否则长轨迹会被误判成步数不足。
	steps, _, _ := replaySteps(events, 0)
	if countOK(steps) < minOKSteps {
		return
	}

	if _, err := a.d.Distill(ctx, a.request(ctx, runID)); err != nil {
		// 自动沉淀是旁路：报错只记日志，不影响 run，也不 panic。
		a.lg.Printf("distill: 自动沉淀 run %s 失败: %v", runID, err)
	}
}

// request 反查 run → session 拿归属。事件里不带 tenant/user，草稿必须挂到
// 正确的人名下；查不到就留空（单租户兼容模式 / session 已被删）。
func (a *attacher) request(ctx context.Context, runID string) Request {
	req := Request{RunID: runID}
	run, err := a.st.GetRun(ctx, runID)
	if err != nil {
		return req
	}
	req.SessionID = run.SessionID
	if run.SessionID == "" {
		return req
	}
	sess, err := a.st.GetSession(ctx, run.SessionID)
	if err != nil {
		return req
	}
	req.TenantID, req.UserID = sess.TenantID, sess.UserID
	return req
}

// markSeenLocked 记录 runID，已存在返回 false。调用方须持有 a.mu。
func (a *attacher) markSeenLocked(runID string) bool {
	if a.seen[runID] {
		return false
	}
	a.seen[runID] = true
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

// payloadStatus 取 run_state 事件的 status 字段。
func payloadStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	return p.Status
}
