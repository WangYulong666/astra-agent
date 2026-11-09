package insight

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

const (
	diagnoseTimeout = 30 * time.Second
	seenLimit       = 10000
)

// newAttacher 装配订阅并返回停止函数。
func newAttacher(hub *eventbus.Hub, st store.Store, act Activity, dataDir string, lg *log.Logger) func() {
	if lg == nil {
		lg = log.Default()
	}
	if hub == nil || st == nil {
		lg.Printf("insight: hub/store 为空，失败诊断未启动")
		return func() {}
	}

	// 与 Open(dir) 共用同一个 dataDir（app 传的都是 <DataDir>/insights）。
	// sharedStore 保证拿到的是同一个实例：Attach 写入后，API 的 All/Recent 与
	// StartRun 回灌立刻能读到，不会各看一份互不相干的缓存。
	var ins *fileStore
	if s, err := sharedStore(dataDir); err != nil {
		// 打开失败（例如 insights.json 损坏）不该让订阅崩掉：仍订阅并记日志，
		// 本轮诊断只是不落库。
		lg.Printf("insight: 打开诊断存储失败，诊断不落库: %v", err)
	} else {
		ins = s
	}

	ctx, cancel := context.WithCancel(context.Background())
	a := &attacher{
		st: st, act: act, ins: ins, lg: lg,
		ctx: ctx, cancel: cancel,
		seen: make(map[string]struct{}),
	}
	hub.OnGlobal(a.onEvent)
	return a.stop
}

// statePayload 是 runner.finish / RecoverInterrupted 发出的 run_state payload。
// 字段刻意只列需要的：不同终态路径填的字段不一样，缺失按空处理。
type statePayload struct {
	Status     string `json:"status"`
	StopReason string `json:"stopReason"`
	Reason     string `json:"reason"`
	Error      string `json:"error"`
}

type attacher struct {
	st  store.Store // 平台存储：GetRun / Events
	act Activity    // 活动账本窄接口，可能为 nil
	ins *fileStore  // 诊断存储，可能为 nil（打开失败）
	lg  *log.Logger

	ctx    context.Context // attacher 生命周期；stop() 时取消，唤醒在飞诊断
	cancel context.CancelFunc

	mu     sync.Mutex
	seen   map[string]struct{} // runID 幂等表
	order  []string            // seen 的 FIFO 顺序
	closed bool
	wg     sync.WaitGroup
}

// onEvent 在 Publish 的 goroutine 里同步执行：必须快，不阻塞、不 panic。
// 只做解析与去重，真正的诊断丢到独立 goroutine。
func (a *attacher) onEvent(ev store.Event) {
	if ev.RunID == "" || ev.Type != store.EventState {
		return
	}
	var p statePayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return // payload 坏直接忽略
	}
	// 只处理终态；成功的 run 不能记成失败诊断。
	//
	// 注：store.IsTerminal 当前只认 completed/failed/cancelled。timeout 作为
	// status 不会出现在这个集合里（超时信息体现在 stopReason/reason/error
	// 文本，由 Suggestion 的 timeout 分类覆盖）。若将来 store 把 timeout 纳入
	// 终态，这里会自动生效。
	if !store.IsTerminal(p.Status) || p.Status == store.RunCompleted {
		return
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	if !a.markSeenLocked(ev.RunID) {
		a.mu.Unlock()
		return // 同一 run 只诊断一次
	}
	// 在锁内 Add：否则 stop() 可能在 Unlock 与 Add 之间开始 Wait，
	// 误以为没有在飞诊断而提前返回。
	a.wg.Add(1)
	a.mu.Unlock()

	go a.diagnose(ev.RunID, p)
}

// markSeenLocked 参考 internal/memory：seen + order，超过上限按 FIFO 淘汰。
func (a *attacher) markSeenLocked(runID string) bool {
	if _, ok := a.seen[runID]; ok {
		return false
	}
	a.seen[runID] = struct{}{}
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

// stop 置 closed、取消在飞诊断，再等所有 goroutine 收工。
func (a *attacher) stop() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()

	// 先取消生命周期 ctx：尊重 ctx 的慢 Activity 会被立刻唤醒退出，
	// 不让 stop 傻等 30s；再 wg.Wait() 等所有诊断 goroutine 真正结束。
	// app 层在关存储前调 stop（见 app.go），顺序不能反。
	a.cancel()
	a.wg.Wait()
}

// diagnose 组装一条结构化诊断并落库。规则式，不调 LLM。
func (a *attacher) diagnose(runID string, p statePayload) {
	defer a.wg.Done()
	defer func() {
		if v := recover(); v != nil {
			a.lg.Printf("insight: 诊断 run %s 发生 panic: %v", runID, v)
		}
	}()

	// 诊断不受发布方（Publish 调用链）的取消影响：base 用 WithoutCancel。
	// 30s 超时兜底；再用 AfterFunc 把 attacher 的 stop 信号桥接进来，
	// 这样 stop() 能主动 cancel 在飞诊断，wg.Wait() 不必等到超时。
	base := context.WithoutCancel(context.Background())
	ctx, cancel := context.WithTimeout(base, diagnoseTimeout)
	unregister := context.AfterFunc(a.ctx, cancel)
	defer func() {
		unregister()
		cancel()
	}()

	d := Diagnosis{
		RunID:     runID,
		Status:    p.Status,
		CreatedAt: time.Now().UTC(),
	}

	// GetRun 补 SessionID / Error（run.Error 优先）。
	if run, err := a.st.GetRun(ctx, runID); err == nil {
		d.SessionID = run.SessionID
		if run.Error != "" {
			d.Error = run.Error
		}
	} else {
		a.lg.Printf("insight: 读 run %s 失败（诊断仍会生成）: %v", runID, err)
	}
	// 归属从会话反查：run 表没有租户字段（归属永远挂在会话上）。
	// 与 distill.Attach 同一条路径；读不到就留空，后续按「无归属」处理——
	// 宁可这条诊断在租户视图里不出现，也不能把别人的数据挂到一个猜出来的租户下。
	if d.SessionID != "" {
		if sess, err := a.st.GetSession(ctx, d.SessionID); err == nil {
			d.TenantID, d.UserID = sess.TenantID, sess.UserID
		} else {
			a.lg.Printf("insight: 读会话 %s 失败（诊断归属为空）: %v", d.SessionID, err)
		}
	}
	if d.Error == "" {
		d.Error = p.Error
	}
	if d.Error == "" {
		d.Error = p.Reason
	}
	if d.Error == "" {
		d.Error = p.StopReason
	}
	if d.Error == "" {
		d.Error = "status=" + p.Status
	}

	// 事件流里最后的工具名 / 工具调用条数。
	evs, err := a.st.Events(ctx, runID, 0, 0)
	if err != nil {
		a.lg.Printf("insight: 读 run %s 事件失败: %v", runID, err)
	}
	for _, ev := range evs {
		if ev.Type != store.EventToolStart && ev.Type != store.EventToolUpdate {
			continue
		}
		d.ToolCount++
		if name := toolName(ev.Payload); name != "" {
			d.LastTool = name
		}
	}

	// 活动账本摘要（窄接口；nil 时给 0）。
	if a.act != nil {
		if sm, err := a.act.Summary(ctx, runID); err == nil {
			d.Blocked, d.Failed = sm.Blocked, sm.Failed
		} else {
			a.lg.Printf("insight: 读 run %s 活动摘要失败: %v", runID, err)
		}
	}

	// suggest 的兜底文案带 {id} 占位符（"查看 /v1/runs/{id}/activity..."），
	// 落库前替换成真实 runID，否则用户拿到的是一条谁也点不开的假路径。
	d.Suggestion = strings.ReplaceAll(suggest(d.Error), "{id}", runID)

	if a.ins != nil {
		if err := a.ins.append(d); err != nil {
			// 写库失败只记日志，不 panic、不影响主流程。
			a.lg.Printf("insight: 写诊断 run %s 失败: %v", runID, err)
		}
	}
}

// toolName 从 tool_call_start / tool_call_update 的 payload 里取工具名，
// 优先 title、其次 kind、最后 toolCallId（runner.dispatch 的字段见 manager.go）。
func toolName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		Title      string `json:"title"`
		Kind       string `json:"kind"`
		ToolCallID string `json:"toolCallId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Title != "" {
		return p.Title
	}
	if p.Kind != "" {
		return p.Kind
	}
	return p.ToolCallID
}

type suggestionRule struct {
	keywords []string // 全小写；大小写不敏感子串匹配
	advice   string
}

// suggestionRules 是有序的：第一个命中的规则生效。不调 LLM，纯规则式。
var suggestionRules = []suggestionRule{
	{[]string{"command not found", "not found in $path", "no such file"},
		"沙箱内缺少该命令；检查 requires 或把工具装进镜像"},
	{[]string{"permission denied", "not permitted", "operation not permitted"},
		"被沙箱或审批拒绝；检查权限策略与出网白名单"},
	{[]string{"egress", "出网被拒", "deny"},
		"出网被策略拦截；在 /v1/admin/egress/policy 加域名白名单或提交审批"},
	{[]string{"timeout", "deadline", "context deadline exceeded"},
		"执行超时；拆小任务或调大超时"},
	{[]string{"401", "403", "unauthorized", "凭据"},
		"凭据无效或过期；重新登录或检查会话级沙箱凭据"},
	{[]string{"llm", "model", "upstream", "502"},
		"模型上游不可用；检查网关 key 与上游地址"},
}

const fallbackSuggestion = "查看 /v1/runs/{id}/activity 定位最后一步，再决定是否沉淀为 skill"

func suggest(errText string) string {
	lower := strings.ToLower(errText)
	for _, r := range suggestionRules {
		for _, kw := range r.keywords {
			if strings.Contains(lower, kw) {
				return r.advice
			}
		}
	}
	return fallbackSuggestion
}
