package schedule

// 调度器主体：一个 goroutine + 每 Tick 扫一次 Due()。
//
// 触发流程刻意分成「先落盘认领，再创建 run」两步：
//
//	claim:  CAS 把 NextRunAt 从 A 推到 B，成功后写回文件
//	run:    CreateRun(ctx, sessionID, prompt)
//	mark:   回写 LastRunAt / LastRunID / LastStatus
//
// 顺序反了就一定会重复触发：run 建好了但进程在写 NextRunAt 前崩掉，
// 下次启动这条还是「到期」状态，再跑一次。先推进时间最多是「丢一次」，
// 丢一次可以人工补，重复触发会把用户的工作区搅乱。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 最近一次触发的状态值。
const (
	statusCreated = "created"
	statusFailed  = "failed"
)

// dueLimit 是单次 Tick 最多处理多少条。
// 加上限是为了「一万条同时到期」时不会一次性卡住整个循环。
const dueLimit = 200

// PromptRunner 是 Scheduler 的可选扩展：触发时在 prompt 后面追加一段上下文。
//
// webhook 的请求体就靠它带进 run——Scheduler 接口是冻结的，
// 只能以「可选接口 + 类型断言」的方式补，不破坏既有实现。
type PromptRunner interface {
	RunNowWithPrompt(ctx context.Context, scheduleID, extraPrompt string) (string, error)
}

type scheduler struct {
	st     *fileStore
	runner Runner
	tick   time.Duration
	lease  time.Duration

	// now 可注入，测试里把时间钉住才能断言 NextRunAt 的推进。
	now func() time.Time

	mu       sync.Mutex
	started  bool
	stopped  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	inflight map[string]time.Time // schedule ID → 租约到期时刻

	baseCtx context.Context
	cancel  context.CancelFunc
}

var _ Scheduler = (*scheduler)(nil)

// Open 装配调度器与持久化。dataDir 是 schedules.json 所在目录。
func Open(cfg Config, runner Runner, dataDir string) (Store, Scheduler, error) {
	if !cfg.Enabled {
		return nil, nil, ErrDisabled
	}
	if runner == nil {
		return nil, nil, fmt.Errorf("%w：缺少 runner", ErrInvalid)
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, nil, fmt.Errorf("%w：dataDir 为空", ErrInvalid)
	}

	tick := cfg.Tick
	if tick <= 0 {
		tick = 30 * time.Second
	}
	lease := cfg.LeaseTTL
	if lease <= 0 {
		lease = 90 * time.Second
	}
	// 租约必须明显长于扫描间隔，否则同一批到期条目会在租约过期后
	// 被本进程再扫一遍（多副本时更容易看出这个抖动）。
	if lease < 3*tick {
		lease = 3 * tick
	}
	maxPerTenant := cfg.MaxPerTenant
	if maxPerTenant <= 0 {
		maxPerTenant = 50
	}

	st, err := openFileStore(dataDir, maxPerTenant)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return st, &scheduler{
		st:       st,
		runner:   runner,
		tick:     tick,
		lease:    lease,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		inflight: make(map[string]time.Time),
		baseCtx:  ctx,
		cancel:   cancel,
	}, nil
}

func (s *scheduler) Enabled() bool { return true }

// Start 起后台循环。重复调用无副作用；Stop 之后再 Start 也是空操作
// （不重启：调度器的生命周期跟着进程走）。
func (s *scheduler) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	s.wg.Add(1)
	go s.loop()
}

// Stop 关循环并等 goroutine 退出。会先取消上下文，
// 这样正在 CreateRun 的那一次能尽快返回，不会把 Stop 卡住一整个 Tick。
func (s *scheduler) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.stopped = true
	started := s.started
	s.mu.Unlock()

	s.cancel()
	if started {
		close(s.stopCh)
	}
	s.wg.Wait()
}

func (s *scheduler) loop() {
	defer s.wg.Done()
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			// 单次 Tick 的超时取租约长度：既是「一条 run 最多建多久」，
			// 也保证 Stop 最多等这么久。
			ctx, cancel := context.WithTimeout(s.baseCtx, s.lease)
			_, _ = s.Tick(ctx)
			cancel()
		}
	}
}

// Tick 扫一次到期条目，返回本次真正触发了几条。
//
// 同步实现：既能被循环调用，也能被测试直接调。
func (s *scheduler) Tick(ctx context.Context) (int, error) {
	now := s.now()
	due, err := s.st.Due(ctx, now, dueLimit)
	if err != nil {
		return 0, err
	}
	triggered := 0
	var errs []error
	for _, it := range due {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		// webhook 型没有 NextRunAt，靠 token 触发；即便被手改出 NextRunAt 也不按时间跑。
		if it.Trigger != TriggerCron {
			continue
		}
		if s.inLease(it.ID, now) {
			continue
		}
		fired, err := s.fire(ctx, it, now)
		if err != nil {
			errs = append(errs, err)
		}
		if fired {
			triggered++
		}
	}
	return triggered, errors.Join(errs...)
}

// fire 触发一条。fired 表示「这条被我抢到了」，与跑成功与否无关——
// 抢到但 CreateRun 失败，仍然算这次 Tick 触发过（不能下个 Tick 再来一遍）。
func (s *scheduler) fire(ctx context.Context, it Schedule, now time.Time) (bool, error) {
	spec, err := parseCron(it.Cron, it.Timezone)
	if err != nil {
		// 表达式不合法只可能来自手改文件（Create/Update 都会校验）。
		// 不推进的话每 Tick 都会撞一次，日志刷爆；推到一天后等人发现。
		return false, s.breakSchedule(ctx, it.ID, now, err)
	}
	next, err := spec.Next(now)
	if err != nil {
		return false, s.breakSchedule(ctx, it.ID, now, err)
	}
	claimed, err := s.st.claim(ctx, it.ID, it.NextRunAt, next)
	if errors.Is(err, ErrNotFound) {
		return false, nil // 扫到之后被删了，正常
	}
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil // 别的副本（或别的 Tick）先抢到了
	}
	s.markInflight(it.ID, now)
	defer s.clearInflight(it.ID)
	return true, s.trigger(ctx, it, now)
}

// breakSchedule 把一条坏掉的 schedule 推到一天后并标 failed，避免死循环重试。
func (s *scheduler) breakSchedule(ctx context.Context, id string, now time.Time, cause error) error {
	_, err := s.st.mutate(ctx, id, func(cur *Schedule) error {
		cur.NextRunAt = now.Add(24 * time.Hour)
		cur.LastStatus = statusFailed
		return nil
	})
	if err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("schedule %s 表达式无法解析，已推迟 24h: %w", id, cause)
}

// trigger 创建 run 并回写结果。
func (s *scheduler) trigger(ctx context.Context, it Schedule, now time.Time) error {
	runID, runErr := s.runner.CreateRun(ctx, it.SessionID, it.Prompt)
	_, markErr := s.st.mutate(ctx, it.ID, func(cur *Schedule) error {
		// 失败也推进 LastRunAt：这是「上次尝试」，配合 LastStatus 才能看出
		// 是「没跑过」还是「跑了但建 run 失败」。NextRunAt 早在 claim 时就推过了。
		cur.LastRunAt = now
		if runErr != nil {
			cur.LastStatus = statusFailed
			cur.LastRunID = ""
			return nil
		}
		cur.LastStatus = statusCreated
		cur.LastRunID = runID
		return nil
	})
	if errors.Is(markErr, ErrNotFound) {
		markErr = nil // 跑的过程中被删了：run 已经建出来，不必惊慌
	}
	if runErr != nil {
		return errors.Join(fmt.Errorf("schedule %s 创建 run 失败: %w", it.ID, runErr), markErr)
	}
	return markErr
}

// RunNow 立即触发一次（手动测试 / webhook）。不动 NextRunAt。
func (s *scheduler) RunNow(ctx context.Context, scheduleID string) (string, error) {
	return s.RunNowWithPrompt(ctx, scheduleID, "")
}

// RunNowWithPrompt 在 RunNow 基础上给 prompt 追加一段上下文（webhook 请求体）。
//
// 只对这一次生效，不写回文件：hook 的载荷不该污染用户的 prompt 模板。
func (s *scheduler) RunNowWithPrompt(ctx context.Context, scheduleID, extraPrompt string) (string, error) {
	it, err := s.st.Get(ctx, scheduleID)
	if err != nil {
		return "", err
	}
	now := s.now()
	prompt := it.Prompt
	if extra := strings.TrimSpace(extraPrompt); extra != "" {
		prompt += "\n\n[webhook payload]\n" + extra
	}
	runID, runErr := s.runner.CreateRun(ctx, it.SessionID, prompt)
	if runErr != nil {
		_, markErr := s.st.mutate(ctx, scheduleID, func(cur *Schedule) error {
			cur.LastRunAt = now
			cur.LastStatus = statusFailed
			cur.LastRunID = ""
			return nil
		})
		return "", errors.Join(runErr, markErr)
	}
	// 元数据回写失败（含「跑的过程中被删了」）不向调用方报错：
	// run 已经建出来了，报错只会让 webhook 调用方重试、建出第二个 run。
	_, _ = s.st.mutate(ctx, scheduleID, func(cur *Schedule) error {
		cur.LastRunAt = now
		cur.LastStatus = statusCreated
		cur.LastRunID = runID
		return nil
	})
	return runID, nil
}

// ── 租约（在途保护）─────────────────────────────────────────

func (s *scheduler) markInflight(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight[id] = now.Add(s.lease)
}

func (s *scheduler) clearInflight(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

// inLease 判断这条是否还在「上一次触发还没结束」的窗口里。
//
// 这是进程内的第二道闸：CAS 保证同一条不会被两个 Tick 各触发一次，
// 但如果 CreateRun 卡住超过一个 Tick，光靠 CAS 会在 NextRunAt 到期后
// 再触发一次，同一个会话上叠两个 run。超过租约就放行——
// 长期卡死是 runner 的问题，不该让调度器永久停掉这条任务。
func (s *scheduler) inLease(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.inflight[id]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(s.inflight, id)
		return false
	}
	return true
}
