package lifecycle

// 回收器实现。
//
// 语义基准来自 D9：**先判「有主」，再判「过期」**，且销毁的永远只是沙箱资源。
// runner.DeleteSession 的顺序是「标记 deleted → 取消 run → Destroy 沙箱 → 删数据」，
// 说明「销毁沙箱」与「删数据」是两件独立的事，删数据只在用户显式要求时发生。
// 回收器只做前者——会话是用户资产，TTL 到期不等于用户可以失去历史记录。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// 回收原因。只用于计数归类与错误摘要，不对外暴露——Report 里的计数已经够用，
// 多一个字符串字段只会让前端多写一个 switch。
const (
	reasonOrphan  = "orphan"
	reasonExpired = "expired"
	reasonIdle    = "idle"
)

// Reaper 按 Policy 回收沙箱。并发安全。
type Reaper struct {
	pol  Policy
	deps Deps
	log  printer

	// Now 时钟。默认 time.Now；测试可替换以固定「现在」。
	// 之所以加在结构体上而不是包级变量：包级变量会让同包内并行跑的测试互相污染，
	// 而且 -race 下改包级变量本身就是竞态。
	Now func() time.Time

	// mu 同时保护 last/lastOK 与「同一时刻只有一轮扫描」。
	//
	// 串行化而不是 TryLock 跳过：手动触发的那个调用者（管理后台的「立即清理」）
	// 期待拿到真实结果，跳过只会返回一份空报告，让人以为「没有可清理的东西」。
	// 后台循环被顶到也不损失正确性——下一拍很快就来。
	mu     sync.Mutex
	last   Report
	lastOK bool
}

// printer 与 Deps.Logger 的方法集一致。
// 单独起个名字只是为了能在 Reaper 里存一个非 nil 的默认值。
type printer interface {
	Printf(format string, v ...any)
}

// New 构造回收器。不启动任何后台任务。
func New(p Policy, deps Deps) *Reaper {
	lg := deps.Logger
	if lg == nil {
		// 必须兜底：Deps.Logger 是接口，为 nil 时直接调 Printf 会 panic，
		// 而回收跑在后台 goroutine 里，panic 会带走整个进程。
		lg = log.Default()
	}
	return &Reaper{pol: p, deps: deps, log: lg}
}

// Policy 返回当前策略，供管理接口展示。
func (r *Reaper) Policy() Policy { return r.pol }

// Enabled 报告回收是否真的会动手。
//
// 三个阈值全关 = 治理功能整体关闭。不加这个开关的话，「全零 Policy」会退化成
// 「凡是没有对应会话的沙箱都删」——用户想关掉回收，结果得到一次大扫除。
// 零值的语义是「不启用」，不是「无限制」，这是 Policy 注释里定下的基调。
func (r *Reaper) Enabled() bool {
	return r.pol.SweepInterval > 0 || r.pol.IdleTimeout > 0 || r.pol.MaxAge > 0
}

// LastReport 返回最近一次扫描的报告。从未扫描过时 ok=false。
func (r *Reaper) LastReport() (Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last, r.lastOK
}

// Run 阻塞循环，ctx 取消即退出。
//
// SweepInterval <= 0 直接返回而不是空转：周期为 0 的 ticker 会 panic，
// 而「0 表示不启用」是 Policy 的既定语义，不是「尽量快」。
func (r *Reaper) Run(ctx context.Context) {
	if r.pol.SweepInterval <= 0 {
		r.log.Printf("lifecycle: 未配置扫描周期，后台回收不启动（只能手动 sweep）")
		return
	}

	tk := time.NewTicker(r.pol.SweepInterval)
	defer tk.Stop()

	for {
		// 启动后立刻扫一次，不等第一个 tick：进程重启后残留的沙箱正是最该马上清掉的。
		rep, err := r.Sweep(ctx)
		if err != nil {
			r.log.Printf("lifecycle: 扫描失败: %v", err)
		}
		// 每次扫描固定一行，便于用 grep 观察回收是否在工作。
		// 即使 0 销毁也打：没有日志时无法区分「没东西可清」和「循环没起来」。
		r.log.Printf("lifecycle: 扫描 %d，销毁 %d（孤儿 %d / 超龄 %d / 空闲 %d），保留 %d，错误 %d",
			rep.Scanned, rep.Destroyed, rep.Orphaned, rep.Expired, rep.Idle, rep.Kept, len(rep.Errors))

		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

// Sweep 执行一次扫描并返回报告。
//
// 返回的 error 只表示「整轮无法进行」（枚举沙箱或会话失败）；
// 单个沙箱销毁失败不中断整轮，记在 Report.Errors 里，error 仍为 nil——
// 否则调用方会因为一台机器上清不掉一个沙箱就以为整轮白跑。
//
// 结果必须具名：FinishedAt 与 last 都在 defer 里写。返回值不具名时 defer
// 改的是副本，调用方拿到的会是零值时间戳。
func (r *Reaper) Sweep(ctx context.Context) (rep Report, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	rep.StartedAt = r.clock()
	defer func() {
		rep.FinishedAt = r.clock()
		r.last, r.lastOK = rep, true
	}()

	if r.deps.Provider == nil || r.deps.Store == nil {
		return rep, fmt.Errorf("lifecycle: 回收器缺少 Provider 或 Store")
	}

	infos, err := r.deps.Provider.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("枚举沙箱失败: %w", err)
	}
	// 一次取全量会话再建索引，而不是对每个沙箱调 GetSession：
	// provider 里可能有成千上万个孤儿，逐个查库会把一次扫描变成 N 次 round-trip。
	sessions, err := r.deps.Store.ListSessions(ctx)
	if err != nil {
		return rep, fmt.Errorf("枚举会话失败: %w", err)
	}

	bySandbox := make(map[string]store.Session, len(sessions))
	for _, s := range sessions {
		key := s.SandboxID
		if key == "" {
			// 历史数据里 SandboxID 可能缺失，此时 ID 就是沙箱 id（见 sandbox_usage.go 的同款处理）。
			key = s.ID
		}
		bySandbox[key] = s
	}

	// 三个阈值全关 = 治理功能整体关闭（见 Enabled）。
	enabled := r.Enabled()

	now := r.clock()
	rep.Scanned = len(infos)

	for _, info := range infos {
		sess, linked := bySandbox[info.ID]

		reason := ""
		switch {
		case !enabled:
			// 什么都不判，全部保留。
		case !linked:
			// 1. 孤儿：provider 有、store 没有。会话可能被直接删了（没走 DeleteSession），
			//    也可能是上一个 provider 留下的残留，没人会再来认领。
			reason = reasonOrphan
		case sess.Status != store.SessionActive:
			// 2. 会话已删 / 已归档 / 处于 idle：绑定关系已经失效，沙箱留着只会烧资源。
			//    注意：这里**只销毁沙箱**，会话记录照旧留着——Deleted 状态本身就是墓碑，
			//    删掉记录会让审计和「这对话曾经存在」的事实一起消失。
			reason = reasonOrphan
		case r.pol.MaxAge > 0 && !info.CreatedAt.IsZero() && now.Sub(info.CreatedAt) > r.pol.MaxAge:
			// 3. 超龄：与活跃度无关的硬上限，兜底「挂着不关」和 agent 死循环。
			reason = reasonExpired
		case r.pol.IdleTimeout > 0 && !sess.LastActiveAt.IsZero() && now.Sub(sess.LastActiveAt) > r.pol.IdleTimeout:
			// 4. 空闲：用户没在说话。LastActiveAt 为零（老数据没写过）时不判空闲——
			//    把「不知道」当成「很久没动」会删掉活得好好的沙箱。
			reason = reasonIdle
		}

		if reason == "" {
			rep.Kept++
			continue
		}

		if err := r.deps.Provider.Destroy(ctx, info.ID); err != nil && !isGone(err) {
			// 单个失败不影响其它沙箱：继续扫描，把摘要留给调用方/日志。
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s（%s）: %v", info.ID, reason, err))
			continue
		}

		rep.Destroyed++
		switch reason {
		case reasonOrphan:
			rep.Orphaned++
		case reasonExpired:
			rep.Expired++
		case reasonIdle:
			rep.Idle++
		}
		// 刻意不调 Store.DeleteSession：沙箱是资源，会话是数据。
		// 回收的触发条件是「没人用了」，而不是「用户要求删除」——后者只有
		// DELETE /v1/sessions/{id} 能做，且不可逆。
	}

	return rep, nil
}

func (r *Reaper) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// isGone 判断「要销毁的东西已经不在了」。
// 这种情形算清理成功：目标状态已达成，记成错误只会让报告里长期挂着一堆噪音。
func isGone(err error) bool {
	return errors.Is(err, sandbox.ErrNotFound)
}
