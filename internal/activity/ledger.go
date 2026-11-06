package activity

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// ledger 把同一 run 的四类素材（事件流 / run 元数据 / 出网日志 / 子任务）
// 聚成一条按时间升序、带 status 的可读时间线。
//
// 容错是硬要求：四类来源里任何一个报错都只记一句日志并跳过，绝不向上抛 error。
// 账本一旦变成 503，用户就什么时间线都看不到；少一类素材只是少几行，远比全空好。
// 因此 ForRun / ForSession / Summary 的错误返回永远是 nil。
type ledger struct {
	st  RunReader
	src Sources
}

// openImpl 必须返回非 nil Ledger：app 层靠 Open 的返回值判断这个能力是否启用
// （见 internal/app/capabilities.go 的 openActivity）。装配本身不会失败。
func openImpl(st RunReader, src Sources) (Ledger, error) {
	return &ledger{st: st, src: src}, nil
}

func (l *ledger) ForRun(ctx context.Context, runID string, limit int) ([]Entry, error) {
	entries := l.buildRunEntries(ctx, runID, egressFetchLimit(limit))
	// 「时间线要看最近的」：按时间升序排好后，截的是最后 limit 条，不是最前面。
	return truncateLast(entries, limit), nil
}

func (l *ledger) ForSession(ctx context.Context, sessionID string, limit int) ([]Entry, error) {
	if l.st == nil {
		return []Entry{}, nil
	}
	runs, err := l.st.ListRuns(ctx, sessionID)
	if err != nil {
		log.Printf("activity: 读取会话 %s 的 run 列表失败，跳过该来源: %v", sessionID, err)
		return []Entry{}, nil
	}

	var all []Entry
	for _, r := range runs {
		// 每个 run 都按 session 级 limit 截断。这样合并后「全局最后 limit 条」
		// 一定落在各 run「自己的最后 limit 条」里（单个 run 对全局尾部最多贡献
		// limit 条），所以逐 run 截断不会丢掉应该出现的条目。
		e, _ := l.ForRun(ctx, r.ID, limit)
		all = append(all, e...)
	}
	sortEntries(all)
	return truncateLast(all, limit), nil
}

func (l *ledger) Summary(ctx context.Context, runID string) (Summary, error) {
	// 统计走全量（不带 limit）聚合：顶部计数回答的是「这一轮一共怎么样了」，
	// 不能被时间线的「只看最近 N 条」截掉。
	entries := l.buildRunEntries(ctx, runID, egressFetchLimit(0))
	var s Summary
	for _, e := range entries {
		switch e.Status {
		case StatusDone:
			s.Done++
		case StatusRunning:
			s.Running++
		case StatusBlocked:
			s.Blocked++
			// entries 已按时间升序：后遇到的 blocked 就是「最新一条」。
			s.Current = e.Title
		case StatusFailed:
			s.Failed++
		}
	}
	return s, nil
}

// buildRunEntries 聚合四类来源，返回按时间升序的**全量**条目，不做截断。
// egressLimit 单独传：出网日志是全局环形缓冲，Log 一次拿 limit*2 条再按
// RunID 过滤，避免别的 run 的日志串进来（见 egressSource）。
func (l *ledger) buildRunEntries(ctx context.Context, runID string, egressLimit int) []Entry {
	var entries []Entry
	sessionID := ""
	var run store.Run
	runOK := false

	// 来源 2：run 本身。先取到它，才能把 SessionID 回填给其它来源的条目。
	if l.st != nil {
		r, err := l.st.GetRun(ctx, runID)
		if err != nil {
			log.Printf("activity: 读取 run %s 失败，跳过该来源: %v", runID, err)
		} else {
			run, runOK, sessionID = r, true, r.SessionID
		}
	}

	// 来源 1：事件流。
	sawTerminalState := false
	if l.st != nil {
		evs, err := l.st.Events(ctx, runID, 0, 0)
		if err != nil {
			log.Printf("activity: 读取 run %s 事件流失败，跳过该来源: %v", runID, err)
		} else {
			ee, terminal := eventEntries(evs, runID, sessionID)
			entries = append(entries, ee...)
			sawTerminalState = terminal
		}
	}

	// run 的 Input 生成一条 KindPlan；终态若事件流里没有对应 run_state 再补一条。
	if runOK {
		entries = append(entries, planEntry(run))
		if store.IsTerminal(run.Status) && !sawTerminalState {
			entries = append(entries, terminalStateEntry(run))
		}
	}

	// 来源 3：出网日志。
	entries = append(entries, egressSource(ctx, l.src.Egress, runID, egressLimit)...)

	// 来源 4：子任务。
	entries = append(entries, childSource(ctx, l.src.Children, runID, sessionID)...)

	sortEntries(entries)
	return entries
}

// egressSource 只保留 RunID 等于当前 run 的日志。Log 是全局缓冲（按租户或
// 全量），不按 run 过滤会把别的任务的出网记录串进这条时间线里。
func egressSource(ctx context.Context, src EgressLog, runID string, limit int) []Entry {
	if src == nil {
		return nil
	}
	logs, err := src.Log(ctx, "", limit)
	if err != nil {
		log.Printf("activity: 读取出网日志失败，跳过该来源: %v", err)
		return nil
	}
	out := make([]Entry, 0, len(logs))
	for _, e := range logs {
		if e.RunID != runID {
			continue
		}
		out = append(out, Entry{
			Time:      e.Time,
			Kind:      KindEgress,
			Title:     egressTitle(e.Host),
			Detail:    egressDetail(e),
			Status:    egressStatus(e.Decision),
			Ref:       e.Host, // 出网记录的原定位是 host
			RunID:     runID,
			SessionID: e.SessionID,
		})
	}
	return out
}

// childSource 把 collab 的子任务记录映射成 KindChild 条目。
func childSource(ctx context.Context, src ChildLister, runID, sessionID string) []Entry {
	if src == nil {
		return nil
	}
	kids, err := src.List(ctx, runID)
	if err != nil {
		log.Printf("activity: 读取 run %s 子任务失败，跳过该来源: %v", runID, err)
		return nil
	}
	out := make([]Entry, 0, len(kids))
	for _, c := range kids {
		t := c.EndedAt
		if t.IsZero() {
			// 仍在跑的子任务没有 EndedAt，放到「现在」附近，别排到时间线最前面。
			t = time.Now()
		}
		out = append(out, Entry{
			Time:      t,
			Kind:      KindChild,
			Title:     childTitle(c.Title),
			Detail:    childDetail(c.Summary, c.Error, c.Status),
			Status:    statusFromString(c.Status, c.Error),
			Ref:       c.RunID,
			RunID:     runID,
			SessionID: sessionID,
		})
	}
	return out
}

func planEntry(run store.Run) Entry {
	title := shortLine(run.Input)
	if title == "" {
		title = "（空指令）"
	}
	return Entry{
		Time: run.CreatedAt, Kind: KindPlan, Title: title, Status: StatusDone,
		RunID: run.ID, SessionID: run.SessionID,
	}
}

func terminalStateEntry(run store.Run) Entry {
	t := run.EndedAt
	if t.IsZero() {
		t = time.Now()
	}
	detail := run.Error
	if detail == "" {
		detail = run.StopReason
	}
	return Entry{
		Time: t, Kind: KindState, Title: stateTitle(run.Status),
		Detail: detail, Status: StatusDone, RunID: run.ID, SessionID: run.SessionID,
	}
}

// egressFetchLimit：ForRun 传 limit*2（过滤后仍能覆盖 limit 条）；
// Summary / 无 limit 时给一个固定上限，避免把整个环形缓冲拉进内存。
func egressFetchLimit(limit int) int {
	if limit > 0 {
		return limit * 2
	}
	return 400
}

func sortEntries(entries []Entry) {
	// SliceStable：同一时刻的事件保持事件流里的原始次序（seq 顺序）。
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Time.Before(entries[j].Time)
	})
}

func truncateLast(entries []Entry, limit int) []Entry {
	if limit > 0 && len(entries) > limit {
		return entries[len(entries)-limit:]
	}
	return entries
}

// ── 文本小工具 ────────────────────────────────────────────────

func stateTitle(status string) string {
	switch status {
	case store.RunCompleted:
		return "已完成"
	case store.RunFailed:
		return "运行失败"
	case store.RunCancelled:
		return "已停止"
	case store.RunWaitingApproval:
		return "等待审批"
	case store.RunRunning:
		return "执行中"
	case store.RunQueued:
		return "排队中"
	default:
		return "状态更新"
	}
}

func egressTitle(host string) string {
	if h := strings.TrimSpace(host); h != "" {
		return "出网请求 · " + h
	}
	return "出网请求"
}

func egressDetail(e EgressEntry) string {
	label := ""
	switch strings.ToLower(strings.TrimSpace(e.Decision)) {
	case "allow", "allowed":
		label = "已放行"
	case "deny", "denied":
		label = "已拦截"
	case "ask":
		label = "等待审批"
	default:
		label = e.Decision
	}
	if e.Reason != "" {
		return label + "：" + e.Reason
	}
	return label
}

// egressStatus：deny 是硬边界（拦截即失败），allow 是已完成；
// ask 属于「等人决定」，落到 blocked；其余未知值保守按 blocked，不冒充成功。
func egressStatus(decision string) string {
	switch strings.ToLower(strings.TrimSpace(decision)) {
	case "deny", "denied":
		return StatusFailed
	case "allow", "allowed":
		return StatusDone
	default:
		return StatusBlocked
	}
}

func childTitle(title string) string {
	if t := strings.TrimSpace(title); t != "" {
		return truncateRunes(t, 80)
	}
	return "子任务"
}

func childDetail(summary, errText, status string) string {
	for _, s := range []string{summary, errText} {
		if strings.TrimSpace(s) != "" {
			return truncateRunes(s, 200)
		}
	}
	return status
}

// statusFromString 把来源里的自由 status 收敛到账本的四个取值。
// err 非空是明确的失败证据，优先于 status（有的子任务 status 还停在 running
// 但已经写了 error）。
func statusFromString(status, errText string) string {
	if strings.TrimSpace(errText) != "" {
		return StatusFailed
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "denied", "rejected", "cancelled", "canceled", "timeout":
		return StatusFailed
	case "completed", "complete", "success", "succeeded", "ok", "done", "allowed", "allow":
		return StatusDone
	default:
		// pending / in_progress / running / 空：没有终态证据，视为进行中。
		return StatusRunning
	}
}

func shortLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return truncateRunes(s, 80)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
