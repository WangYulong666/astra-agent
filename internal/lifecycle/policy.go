// Package lifecycle 管沙箱的「后半生」：回收、清理、配额。
//
// 为什么单独成包而不是塞进 runner：runner 关心的是「一次 run 怎么跑完」，
// 而回收关心的是「没人在跑的沙箱怎么消失」。两者共享 provider 与会话表，
// 但触发时机、失败语义完全不同——回收失败了不该影响任何正在进行的对话，
// 混在一个包里早晚会互相污染锁与错误处理。
package lifecycle

import (
	"context"
	"time"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// Policy 回收与配额策略。零值表示「该项不启用」，不是「用默认值」——
// 默认值会让「我想关掉空闲回收」变成「我配了 0 结果它按 30 分钟删我沙箱」。
type Policy struct {
	// SweepInterval 扫描周期。0 时不启动后台循环（只能手动 Sweep）。
	SweepInterval time.Duration

	// IdleTimeout 会话无活动超过此值即回收。判据是 Session.LastActiveAt。
	IdleTimeout time.Duration

	// MaxAge 沙箱从创建起的最长存活时间，与是否活跃无关。
	// 用于兜底「用户挂着不关」与「agent 失控循环」。
	MaxAge time.Duration

	// MaxPerTenant 单租户并发沙箱上限。0 不限。
	// 这是**准入控制**：超限时拒绝新会话，而不是事后踢掉旧的。
	MaxPerTenant int
}

// Deps 回收器依赖。分开传是为了让测试能只替换其中一项。
type Deps struct {
	Provider sandbox.Provider
	Store    store.Store
	// WithIdleSession 与 runner 的执行锁互斥，避免检查空闲后恰好开始执行。
	WithIdleSession func(context.Context, string, func() error) (bool, error)
	// Logger 为空时用 log.Default()。
	Logger interface {
		Printf(format string, v ...any)
	}
}

// Report 单次扫描的结果。字段都是计数，便于打日志与断言。
type Report struct {
	Scanned    int      // 枚举到的沙箱总数
	Paused     int      // 停止运行但保留工作区和 agent home
	Destroyed  int      // 本次销毁数
	Orphaned   int      // 其中因「无对应会话」被销毁的
	Expired    int      // 其中因 MaxAge 到期回收的（含暂停）
	Idle       int      // 其中因空闲超时回收的（含暂停）
	Kept       int      // 判定保留
	Errors     []string // 单个沙箱处理失败的记录；不中断整轮扫描
	StartedAt  time.Time
	FinishedAt time.Time
}

// Quota 准入控制。与 Reaper 分开：配额是同步判断（创建会话前），
// 回收是异步兜底，两者不能互为替代。
type Quota struct {
	Provider sandbox.Provider
	Store    store.Store
	// MaxPerTenant 0 表示不限。
	MaxPerTenant int
}

// ErrQuotaExceeded 租户并发沙箱超限。
//
// 用错误值而不是 bool，是为了让上层能把它映射成 429/403 并带上明确文案，
// 而不是笼统的「创建失败」。
var ErrQuotaExceeded = quotaError("租户并发沙箱数已达上限")

type quotaError string

func (e quotaError) Error() string { return string(e) }
