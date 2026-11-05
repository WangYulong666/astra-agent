package obs

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 审计写入的两个取舍，是这一整个文件存在的理由：
//
//  1. **异步**：api 层的 h.audit() 是在请求路径上同步调用的。如果在这里同步落库，
//     一次慢查询（锁等待、磁盘抖动）就会把用户请求一起拖住几十秒——审计是旁路，
//     不该有能力拖慢主流程。所以 Audit 只做一次非阻塞的入队。
//  2. **超时**：入队之后由后台 worker 落库，每次写入带 3s 超时。数据库卡死时，
//     宁可丢一条审计也不能让 worker 永久挂在那条 SQL 上（那样队列很快会被塞满，
//     后面的审计全部丢失，且进程关不掉）。
//
// 代价：进程被 kill -9 时，队列里还没落库的审计会丢。这是刻意的——
// 审计的完整性靠数据库事务与「写前不返回」来保证，但这里连主流程都不阻塞，
// 就只能接受这个窗口。要强一致的话，得把审计写进业务事务，那是另一套设计。

const (
	// defaultAuditTimeout 单条审计的写入上限。
	// 3s 的依据：正常 SQLite/PG 单行 insert 在毫秒级，3s 已经覆盖了
	// 「库在但很慢」的抖动；再长就不值得为一个旁路等待了。
	defaultAuditTimeout = 3 * time.Second

	// auditQueueSize 队列深度。约等于「数据库抖动 3s 期间能攒下的审计条数」。
	// 满了就丢并打 warn——丢审计好过把内存吃光。
	auditQueueSize = 512

	// auditWorkers 并发写入数。>1 是因为一条 3s 的慢查询不该堵住整条审计流；
	// 顺序只影响审计行的先后，CreatedAt 本来就记录了真实时间。
	auditWorkers = 4
)

// Auditor 实现 api.Auditor。
//
// 注意接口是在 api 包定义的，这里靠方法签名满足它——obs 不 import api，
// 否则 api → obs（metricsHandler 要用 Handler）与 obs → api 会形成循环依赖。
type Auditor struct {
	st      store.AdminStore
	lg      *slog.Logger
	timeout time.Duration

	ch   chan store.AuditLog
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// 编译期确认接口形状。api.Auditor 是结构化满足，写不出断言（会 import api），
// 所以这里断言的是一模一样的方法签名——签名漂移时这一行会先编译失败。
var _ interface {
	Audit(ctx context.Context, a store.AuditLog)
} = (*Auditor)(nil)

// NewAuditor 构造审计器并启动后台 worker。
//
// st 为 nil 时返回一个「只打日志不落库」的审计器：本地 file store 模式下没有
// AdminStore，但审计日志本身仍然有价值（谁在什么时候动了什么）。
func NewAuditor(st store.AdminStore, lg *slog.Logger) *Auditor {
	if lg == nil {
		lg = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	a := &Auditor{
		st:      st,
		lg:      lg,
		timeout: defaultAuditTimeout,
		ch:      make(chan store.AuditLog, auditQueueSize),
		stop:    make(chan struct{}),
	}
	if st != nil {
		for i := 0; i < auditWorkers; i++ {
			a.wg.Add(1)
			go a.worker()
		}
	}
	return a
}

// Audit 记录一条审计。永不阻塞、永不 panic。
//
// ctx 只用来取「调用方是否已经放弃」以外的信息（事实上这里刻意不使用它）：
// 如果跟着请求 ctx 走，请求一结束 ctx 就被取消，审计会被一起取消掉
// ——恰恰是「用户点了取消/断线」这种最需要留痕的场景丢记录。
func (a *Auditor) Audit(_ context.Context, rec store.AuditLog) {
	if a == nil {
		return
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}

	// 无论是否落库，都先打一条结构化日志：库不可用时，这是唯一的事实来源。
	a.lg.Info("audit",
		"action", rec.Action,
		"target_type", rec.TargetType,
		"target_id", dashIfEmpty(rec.TargetID),
		"tenant", dashIfEmpty(rec.TenantID),
		"user", dashIfEmpty(rec.UserID),
		"actor", dashIfEmpty(rec.ActorEmail),
		"ip", dashIfEmpty(rec.IP),
	)

	if a.st == nil {
		return // 没有治理层存储：日志即审计。
	}

	select {
	case a.ch <- rec:
	case <-a.stop:
		// 关停中，丢弃。
	default:
		// 队列满：丢最旧的没有意义（那会让审计顺序错乱到更难解释），
		// 丢当前这条并亮 warn，让人能从日志里发现「审计有缺口」。
		a.lg.Warn("audit 队列已满，丢弃一条审计", "action", rec.Action, "tenant", rec.TenantID)
	}
}

// Close 停止后台 worker。未落库的审计会被丢弃——理由见文件头。
func (a *Auditor) Close() {
	if a == nil {
		return
	}
	a.once.Do(func() { close(a.stop) })
	a.wg.Wait()
}

func (a *Auditor) worker() {
	defer a.wg.Done()
	for {
		select {
		case <-a.stop:
			return
		case rec := <-a.ch:
			a.insert(rec)
		}
	}
}

// insert 落库。单独一个函数是为了能把 recover 写在一处：
// 存储实现（尤其第三方 SQL 驱动）panic 时，不能让整个 worker goroutine 死掉，
// 否则此后所有审计都会静默消失。
func (a *Auditor) insert(rec store.AuditLog) {
	defer func() {
		if v := recover(); v != nil {
			a.lg.Error("审计写入 panic，已吞掉", "action", rec.Action, "panic", v)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
	defer cancel()

	if err := a.st.InsertAudit(ctx, rec); err != nil {
		// 只记日志。审计失败不是主流程的失败点：调用方（api）此时已经把
		// 200/201 回给用户了，这里再返回错误也没有人能处理。
		a.lg.Error("审计写入失败",
			"action", rec.Action,
			"target_type", rec.TargetType,
			"target_id", rec.TargetID,
			"tenant", rec.TenantID,
			"err", err,
		)
	}
}
