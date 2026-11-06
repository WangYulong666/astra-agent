package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

const (
	defaultMaxDepth = 2
	defaultTimeout  = 5 * time.Minute
	pollInterval    = 200 * time.Millisecond
	summaryLimit    = 2000 // 字符（rune），不是字节，避免切坏 UTF-8

	childFilePerm os.FileMode = 0o600
	childDirPerm  os.FileMode = 0o700
)

// errClosed 是 Close 之后拒绝新 Spawn 的内部错误，不属于冻结契约。
var errClosed = errors.New("collab: 协调器已关闭")

// coordinator 是 Coordinator 的真实现。
//
// children.json 是唯一 source of truth 的内存镜像：所有更新都在 c.mu 写锁下
// 改 map、再全量序列化落盘。并发 Spawn 不会各自读文件追加（那样必写坏），
// 而是串行地「改内存 → 整份重写」，rename 本身原子，结果是 last-write-wins
// 且永远是合法 JSON。
type coordinator struct {
	cfg Config
	st  store.Store

	mu       sync.RWMutex
	children map[string]Child // key = RunID

	// lifeMu 把「检查 closed」与「wg.Add(1)」绑成原子操作：否则 Close 的
	// wg.Wait() 与迟到的 Add 并发，会触发 WaitGroup 的 Add/Wait 竞态 panic。
	lifeMu    sync.Mutex
	closed    bool
	wg        sync.WaitGroup
	closeCh   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func openImpl(cfg Config, st store.Store) (Coordinator, error) {
	if cfg.Sessions == nil {
		return nil, fmt.Errorf("collab: Sessions 未配置")
	}
	if st == nil {
		return nil, fmt.Errorf("collab: Store 未配置")
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("collab: DataDir 未配置")
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = defaultMaxDepth
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if err := os.MkdirAll(cfg.DataDir, childDirPerm); err != nil {
		return nil, fmt.Errorf("collab: 建数据目录失败: %w", err)
	}
	c := &coordinator{
		cfg:      cfg,
		st:       st,
		children: map[string]Child{},
		closeCh:  make(chan struct{}),
	}
	if err := c.load(); err != nil {
		return nil, fmt.Errorf("collab: 读取 children.json 失败: %w", err)
	}
	return c, nil
}

// spawnedPayload / resultPayload 是 EventChildSpawned / EventChildResult 的
// 事件体。字段名与前端约定一致；用 struct 而不是 map，序列化结果稳定。
type spawnedPayload struct {
	ChildRunID     string `json:"childRunId"`
	ChildSessionID string `json:"childSessionId"`
	Input          string `json:"input"`
	Title          string `json:"title"`
}

type resultPayload struct {
	ChildRunID string `json:"childRunId"`
	Status     string `json:"status"`
	Summary    string `json:"summary"`
	Error      string `json:"error"`
}

// waitOutcome 区分「正常终态」与三种提前退出，避免把超时/取消误判成子任务失败。
type waitOutcome int

const (
	outcomeTerminal waitOutcome = iota
	outcomeTimeout
	outcomeCanceled // ctx.Done()
	outcomeClosed   // Close()
)

func (c *coordinator) Spawn(ctx context.Context, req SpawnRequest) (Child, error) {
	// 关闭后不再接活；wg 跟踪每个进行中的 Spawn，供 Close 收敛。
	c.lifeMu.Lock()
	if c.closed {
		c.lifeMu.Unlock()
		return Child{}, errClosed
	}
	c.wg.Add(1)
	c.lifeMu.Unlock()
	defer c.wg.Done()

	// 1. 父 run → 父会话。TenantID/UserID 一律用 req（服务端从会话继承后传入），
	//    绝不从请求体或父会话里另猜一份，否则等于允许往别人租户名下起沙箱。
	parentRun, err := c.st.GetRun(ctx, req.ParentRunID)
	if err != nil {
		return Child{}, fmt.Errorf("collab: 读取父 run: %w", err)
	}
	parentSess, err := c.st.GetSession(ctx, parentRun.SessionID)
	if err != nil {
		return Child{}, fmt.Errorf("collab: 读取父会话: %w", err)
	}

	// 2. 深度检查：沿 store.Run.ParentRunID 往上数边数。
	//    达到 MaxDepth 直接拒绝，且在任何创建动作之前返回，不留半截子会话。
	parentDepth := c.parentDepth(ctx, req.ParentRunID)
	if parentDepth >= c.cfg.MaxDepth {
		return Child{}, ErrTooDeep
	}
	childDepth := parentDepth + 1

	// 3. 子会话（独立沙箱）。
	sessInfo, err := c.cfg.Sessions.CreateSession(ctx, CreateOpts{
		ParentSessionID: parentSess.ID,
		TenantID:        req.TenantID,
		UserID:          req.UserID,
		Title:           req.Title,
	})
	if err != nil {
		return Child{}, fmt.Errorf("collab: 创建子会话: %w", err)
	}

	// 4. 子 run。
	runInfo, err := c.cfg.Sessions.StartRun(ctx, sessInfo.ID, req.Input, req.ParentRunID)
	if err != nil {
		return Child{}, fmt.Errorf("collab: 启动子 run: %w", err)
	}
	childRunID := runInfo.ID

	// Sessions.StartRun 的窄接口不接收 parentRunID，装配用的 adapter 也调的是
	// 不带 parent 的 mgr.StartRun。链要能在运行时自然延伸（子 agent 再派生），
	// 只能由 collab 在这里补写 store.Run.ParentRunID——否则深度防护只挡得住
	// 第一跳，第二跳的父 run 永远查不到祖先，指数派生就拦不住了。
	c.linkParent(ctx, childRunID, req.ParentRunID)

	// 5. 先落 running 再等：等完才落库的话，进程崩在等待中，这个子任务就
	//    永久查不到（子 run 还在跑，父事件流也没有任何派生痕迹）。
	now := time.Now().UTC()
	child := Child{
		SessionID:   sessInfo.ID,
		RunID:       childRunID,
		ParentRunID: req.ParentRunID,
		Title:       req.Title,
		Status:      "running",
		Depth:       childDepth,
		CreatedAt:   now,
	}
	if err := c.addOrUpdate(child); err != nil {
		_ = c.cfg.Sessions.CancelRun(context.WithoutCancel(ctx), childRunID)
		return Child{}, fmt.Errorf("collab: 落库子任务记录: %w", err)
	}

	// bg 不带取消：超时 / ctx 取消 / Close 之后仍要把 Result 事件与 CancelRun
	// 做完，不能被已经 Done 的原 ctx 连带掐掉。
	bg := context.WithoutCancel(ctx)

	// 8a. 派生事件先发，Result 后发；两条 CreatedAt 之差就是「隔多久回话」。
	c.appendEvent(bg, req.ParentRunID, store.EventChildSpawned, spawnedPayload{
		ChildRunID:     childRunID,
		ChildSessionID: sessInfo.ID,
		Input:          req.Input,
		Title:          req.Title,
	})

	// 6. 等终态。
	finalRun, outcome := c.waitTerminal(ctx, childRunID)

	// 7. 摘要：子 run 里所有 message_delta 的 payload.text 拼接、截断。
	summary, sumErr := c.buildSummary(bg, childRunID)
	child.Summary = summary
	child.EndedAt = time.Now().UTC()

	switch outcome {
	case outcomeTimeout:
		child.Status = "timeout"
		child.Error = "子任务等待超时"
		_ = c.cfg.Sessions.CancelRun(bg, childRunID)
	case outcomeCanceled, outcomeClosed:
		child.Status = "cancelled"
		child.Error = "派生已取消"
		_ = c.cfg.Sessions.CancelRun(bg, childRunID)
	default: // outcomeTerminal
		child.Status = finalRun.Status
		if finalRun.Error != "" {
			child.Error = finalRun.Error
		} else if sumErr != nil {
			child.Error = "读取摘要失败: " + sumErr.Error()
		}
	}

	// 8b. 回话事件。所有出口（正常/超时/取消/关闭）都发，父事件流才完整。
	c.appendEvent(bg, req.ParentRunID, store.EventChildResult, resultPayload{
		ChildRunID: childRunID,
		Status:     child.Status,
		Summary:    child.Summary,
		Error:      child.Error,
	})

	// 9. 更新最终状态并落盘。
	if err := c.addOrUpdate(child); err != nil {
		return child, fmt.Errorf("collab: 更新子任务记录: %w", err)
	}

	switch outcome {
	case outcomeTimeout:
		return child, ErrTimeout
	case outcomeCanceled:
		return child, ctx.Err()
	case outcomeClosed:
		return child, errClosed
	default:
		return child, nil
	}
}

// parentDepth 沿 ParentRunID 链往上数边数（根 run 为 0）。
//
// 防御：链断（GetRun 失败）就停，按已数到的深度算；seen 防环；数到
// MaxDepth 立即短路——再往上走只会更长，判定不会改变，也顺带杜绝了超长链。
func (c *coordinator) parentDepth(ctx context.Context, runID string) int {
	seen := make(map[string]bool)
	cur := runID
	depth := 0
	for cur != "" && !seen[cur] {
		seen[cur] = true
		r, err := c.st.GetRun(ctx, cur)
		if err != nil { // 链断了：历史数据不全，按当前深度处理
			break
		}
		if r.ParentRunID == "" { // 到根
			break
		}
		depth++
		cur = r.ParentRunID
		if depth >= c.cfg.MaxDepth {
			break
		}
	}
	return depth
}

// linkParent 给刚创建的子 run 补写 ParentRunID，建立深度链（见 Spawn 注释）。
// best-effort：写不进去不影响本次派生结果，最坏是深度检查少认一跳。
func (c *coordinator) linkParent(ctx context.Context, childRunID, parentRunID string) {
	r, err := c.st.GetRun(ctx, childRunID)
	if err != nil || r.ParentRunID == parentRunID {
		return
	}
	r.ParentRunID = parentRunID
	_ = c.st.UpdateRun(ctx, r)
}

// waitTerminal 轮询 store 直到终态，或 ctx / timeout / Close 触发提前退出。
//
// 用轮询而不是订阅 Hub：终态事件可能在我们订阅之前就已经发出，订阅会永远
// 等下去；store 里的 run.Status 是唯一权威、可重放的真相，200ms 轮询不会
// 漏掉任何已经发生的终态。
func (c *coordinator) waitTerminal(ctx context.Context, runID string) (store.Run, waitOutcome) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()

	var last store.Run
	for {
		// 先立即查一次：fake / 已经跑完的子 run 不该白等一个 tick。
		if r, err := c.st.GetRun(ctx, runID); err == nil {
			last = r
			if store.IsTerminal(r.Status) {
				return r, outcomeTerminal
			}
		}
		select {
		case <-ctx.Done():
			return last, outcomeCanceled
		case <-c.closeCh:
			return last, outcomeClosed
		case <-timer.C:
			return last, outcomeTimeout
		case <-ticker.C:
		}
	}
}

// buildSummary 拼接 run 里所有 message_delta 事件的文本。
//
// payload 的真实结构来自 runner/sink.go 的 flushLocked：
//
//	s.appendLocked(typ, map[string]any{"text": b.String()})
//
// 即 JSON {"text":"..."}，字段名是 text，不是 content / delta / message。
// 坏行 / 非该类型的事件直接跳过；截断按 rune 数，避免把中文切半个字。
func (c *coordinator) buildSummary(ctx context.Context, runID string) (string, error) {
	events, err := c.st.Events(ctx, runID, 0, 0) // afterSeq=0, limit=0 → 全部
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ev := range events {
		if ev.Type != store.EventMessageDelta {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			continue
		}
		b.WriteString(p.Text)
	}
	s := b.String()
	if r := []rune(s); len(r) > summaryLimit {
		s = string(r[:summaryLimit])
	}
	return s, nil
}

// appendEvent 是展示用事件的 best-effort 写入：结果已在 children.json 落库，
// 事件流 Append 失败（父 run 被删等）不该让整次派生失败。
func (c *coordinator) appendEvent(ctx context.Context, parentRunID, typ string, payload any) {
	_, _ = c.st.Append(ctx, parentRunID, store.NewEvent{Type: typ, Payload: payload})
}

func (c *coordinator) List(ctx context.Context, parentRunID string) ([]Child, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Child, 0)
	for _, ch := range c.children {
		if ch.ParentRunID == parentRunID {
			out = append(out, ch)
		}
	}
	// 按 CreatedAt 正序；同一时刻创建的用 RunID 兜底，保证顺序稳定。
	sortChildren(out)
	return out, nil
}

func (c *coordinator) Cancel(ctx context.Context, runID string) error {
	c.mu.RLock()
	ch, ok := c.children[runID]
	c.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	// 只对还在 running 的生效；已终态的记录是 no-op。
	if ch.Status != "running" {
		return nil
	}
	if err := c.cfg.Sessions.CancelRun(ctx, runID); err != nil {
		return err
	}
	ch.Status = "cancelled"
	ch.EndedAt = time.Now().UTC()
	return c.addOrUpdate(ch)
}

// Close 广播 closeCh 让进行中的 waitTerminal 退出，wg.Wait 收敛所有 Spawn，
// 再最后刷一次盘。全程不持有 c.mu 等待，不会和 Spawn 的落库互相锁死。
func (c *coordinator) Close() error {
	c.closeOnce.Do(func() {
		c.lifeMu.Lock()
		c.closed = true
		c.lifeMu.Unlock()

		close(c.closeCh)
		c.wg.Wait()

		c.mu.Lock()
		c.closeErr = c.flushLocked()
		c.mu.Unlock()
	})
	return c.closeErr
}
