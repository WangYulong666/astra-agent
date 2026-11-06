package activity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── 桩：RunReader / EgressLog / ChildLister ─────────────────────

type fakeRunReader struct {
	runs    map[string]store.Run
	events  map[string][]store.Event
	list    map[string][]store.Run
	getErr  error
	evtErr  error
	listErr error
}

func (f *fakeRunReader) GetRun(_ context.Context, id string) (store.Run, error) {
	if f.getErr != nil {
		return store.Run{}, f.getErr
	}
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return store.Run{}, store.ErrNotFound
}

func (f *fakeRunReader) GetSession(_ context.Context, _ string) (store.Session, error) {
	return store.Session{}, store.ErrNotFound
}

func (f *fakeRunReader) ListRuns(_ context.Context, sid string) ([]store.Run, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list[sid], nil
}

func (f *fakeRunReader) Events(_ context.Context, id string, _ int64, _ int) ([]store.Event, error) {
	if f.evtErr != nil {
		return nil, f.evtErr
	}
	return f.events[id], nil
}

type fakeEgress struct {
	entries   []EgressEntry
	err       error
	gotTenant string
	gotLimit  int
}

func (f *fakeEgress) Log(_ context.Context, tenant string, limit int) ([]EgressEntry, error) {
	f.gotTenant, f.gotLimit = tenant, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

type fakeChildren struct {
	entries []ChildEntry
	err     error
}

func (f *fakeChildren) List(_ context.Context, _ string) ([]ChildEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// ── 工具 ───────────────────────────────────────────────────────

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(min int) time.Time { return baseTime.Add(time.Duration(min) * time.Minute) }

func mkEvent(seq int64, typ string, t time.Time, payload map[string]any) store.Event {
	b, _ := json.Marshal(payload)
	return store.Event{Seq: seq, Type: typ, Payload: b, CreatedAt: t}
}

func newLedger(t *testing.T, st RunReader, src Sources) Ledger {
	t.Helper()
	l, err := Open(st, src)
	if err != nil {
		t.Fatalf("Open 返回错误: %v", err)
	}
	if l == nil {
		t.Fatal("Open 返回了 nil Ledger（app 靠它判断能力是否启用）")
	}
	return l
}

func countKind(entries []Entry, kind string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func findKind(entries []Entry, kind string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ── 用例 ───────────────────────────────────────────────────────

// openImpl 必须返回非 nil Ledger，且不再报「未实现」。
func TestOpenReturnsUsableLedger(t *testing.T) {
	st := &fakeRunReader{runs: map[string]store.Run{
		"r1": {ID: "r1", SessionID: "s1", Status: store.RunRunning, Input: "hi", CreatedAt: at(0)},
	}}
	_ = newLedger(t, st, Sources{})
}

// 事件 → 条目的完整映射：message 合并、审批翻转、状态推导、错误、子任务。
func TestEventMapping(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r1": {ID: "r1", SessionID: "s1", Status: store.RunFailed, Input: "帮我做事",
				CreatedAt: at(-1), EndedAt: at(20)},
		},
		events: map[string][]store.Event{
			"r1": {
				mkEvent(1, store.EventMessageDelta, at(0), map[string]any{"text": "我来"}),
				mkEvent(2, store.EventMessageDelta, at(1), map[string]any{"text": "看看"}),
				mkEvent(3, store.EventToolStart, at(2), map[string]any{
					"toolCallId": "tc1", "title": "bash", "kind": "execute", "status": "pending"}),
				mkEvent(4, store.EventToolUpdate, at(3), map[string]any{
					"toolCallId": "tc1", "status": "failed"}),
				mkEvent(5, store.EventApprovalReq, at(4), map[string]any{
					"toolCallId": "tc2", "title": "写文件", "pending": true}),
				mkEvent(6, store.EventApprovalResult, at(5), map[string]any{
					"toolCallId": "tc2", "decision": "allow", "by": "u1"}),
				mkEvent(7, store.EventState, at(6), map[string]any{"status": store.RunWaitingApproval}),
				mkEvent(8, store.EventState, at(7), map[string]any{"status": store.RunRunning}),
				mkEvent(9, store.EventState, at(8), map[string]any{"status": store.RunFailed}),
				mkEvent(10, store.EventError, at(9), map[string]any{"message": "boom"}),
				mkEvent(11, store.EventChildSpawned, at(10), map[string]any{
					"runId": "c1", "title": "子任务A"}),
				mkEvent(12, store.EventMessageDelta, at(11), map[string]any{"text": "又一句"}),
			},
		},
	}
	l := newLedger(t, st, Sources{})
	entries, err := l.ForRun(context.Background(), "r1", 0)
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}

	// 连续 delta 合并成 1 条，后面的 delta 另起 1 条（中间被非 delta 事件打断）。
	msgs := findKind(entries, KindMessage)
	if len(msgs) != 2 {
		t.Fatalf("KindMessage 条数 = %d，应为 2（连续合并 + 断点后另起）", len(msgs))
	}
	if msgs[0].Detail != "我来看看" {
		t.Errorf("首条消息应合并为 %q，实际 %q", "我来看看", msgs[0].Detail)
	}
	if msgs[1].Detail != "又一句" {
		t.Errorf("断点后的消息 = %q，应为 %q", msgs[1].Detail, "又一句")
	}

	// plan：来自 run.Input，时间用 CreatedAt。
	plans := findKind(entries, KindPlan)
	if len(plans) != 1 || plans[0].Title != "帮我做事" || !plans[0].Time.Equal(at(-1)) {
		t.Errorf("KindPlan 不对: %+v", plans)
	}

	// tool：start + update 各一条，title 用 title/kind，Ref=toolCallId。
	tools := findKind(entries, KindTool)
	if len(tools) != 2 {
		t.Fatalf("KindTool 条数 = %d，应为 2", len(tools))
	}
	if tools[0].Title != "bash" || tools[0].Ref != "tc1" || tools[0].Status != StatusRunning {
		t.Errorf("tool start 不对: %+v", tools[0])
	}
	if tools[1].Status != StatusFailed {
		t.Errorf("tool update 失败应推导为 failed，实际 %q", tools[1].Status)
	}

	// approval：req(pending) 建 1 条 blocked，result 把它翻成 done；不另起条目。
	approvals := findKind(entries, KindApproval)
	if len(approvals) != 1 {
		t.Fatalf("KindApproval 条数 = %d，应为 1（result 翻同一条 Ref）", len(approvals))
	}
	if approvals[0].Ref != "tc2" || approvals[0].Status != StatusDone {
		t.Errorf("审批没按 Ref 翻成 done: %+v", approvals[0])
	}

	// run_state：waiting_approval→blocked / running→running / terminal→done。
	states := findKind(entries, KindState)
	if len(states) != 3 {
		t.Fatalf("KindState 条数 = %d，应为 3（终态已在事件流里，不该补）", len(states))
	}
	byStatus := map[string]string{}
	for _, s := range states {
		byStatus[s.Title] = s.Status
	}
	if byStatus["等待审批"] != StatusBlocked || byStatus["执行中"] != StatusRunning || byStatus["运行失败"] != StatusDone {
		t.Errorf("run_state status 推导不对: %+v", byStatus)
	}

	// error：failed。child：存在。
	errs := findKind(entries, KindError)
	if len(errs) != 1 || errs[0].Status != StatusFailed || errs[0].Detail != "boom" {
		t.Errorf("KindError 不对: %+v", errs)
	}
	if countKind(entries, KindChild) != 1 {
		t.Errorf("应有一条 KindChild")
	}

	// 全局按时间升序。
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatalf("条目未按时间升序: [%d]=%v < [%d]=%v", i, entries[i].Time, i-1, entries[i-1].Time)
		}
	}
}

// 审批拒绝：result 把 blocked 翻成 failed，而不是 done。
func TestApprovalDenyFlipsToFailed(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r": {ID: "r", SessionID: "s", Status: store.RunRunning, Input: "x", CreatedAt: at(0)},
		},
		events: map[string][]store.Event{
			"r": {
				mkEvent(1, store.EventApprovalReq, at(1), map[string]any{
					"toolCallId": "tc", "pending": true}),
				mkEvent(2, store.EventApprovalResult, at(2), map[string]any{
					"toolCallId": "tc", "decision": "deny", "by": "u"}),
			},
		},
	}
	entries, _ := newLedger(t, st, Sources{}).ForRun(context.Background(), "r", 0)
	ap := findKind(entries, KindApproval)
	if len(ap) != 1 || ap[0].Status != StatusFailed {
		t.Fatalf("拒绝应翻成 failed: %+v", ap)
	}
}

// 出网日志只保留当前 run，且 deny→failed / allow→done；Log 传空 tenant + limit*2。
func TestEgressFiltering(t *testing.T) {
	st := &fakeRunReader{runs: map[string]store.Run{
		"rA": {ID: "rA", SessionID: "s", Status: store.RunRunning, Input: "x", CreatedAt: at(0)},
	}}
	eg := &fakeEgress{entries: []EgressEntry{
		{Time: at(1), RunID: "rA", Host: "a.com", Decision: "allow"},
		{Time: at(2), RunID: "rB", Host: "b.com", Decision: "deny"}, // 别的 run，必须过滤
		{Time: at(3), RunID: "rA", Host: "c.com", Decision: "deny"},
	}}
	l := newLedger(t, st, Sources{Egress: eg})
	entries, err := l.ForRun(context.Background(), "rA", 5)
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}
	if eg.gotTenant != "" {
		t.Errorf("Log 的 tenantID 应传空串，实际 %q", eg.gotTenant)
	}
	if eg.gotLimit != 10 {
		t.Errorf("Log 的 limit 应为 5*2=10，实际 %d", eg.gotLimit)
	}
	egs := findKind(entries, KindEgress)
	if len(egs) != 2 {
		t.Fatalf("KindEgress 条数 = %d，应为 2（过滤掉 rB）", len(egs))
	}
	for _, e := range egs {
		if e.Title == "" || e.Ref == "" {
			t.Errorf("egress 条目缺 host/Ref: %+v", e)
		}
		if e.Detail == "" {
			t.Errorf("egress 条目缺 detail: %+v", e)
		}
	}
	statusByHost := map[string]string{}
	for _, e := range egs {
		statusByHost[e.Ref] = e.Status
	}
	if statusByHost["a.com"] != StatusDone || statusByHost["c.com"] != StatusFailed {
		t.Errorf("egress decision 推导不对: %+v", statusByHost)
	}
	for _, e := range egs {
		if e.Ref == "b.com" {
			t.Fatal("别的 run 的出网日志串进来了")
		}
	}
}

// limit 生效后返回「最后」limit 条，而不是最前面。
func TestLimitReturnsMostRecent(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r": {ID: "r", SessionID: "s", Status: store.RunRunning, Input: "x", CreatedAt: at(0)},
		},
		events: map[string][]store.Event{"r": {
			mkEvent(1, store.EventState, at(1), map[string]any{"status": store.RunRunning}),
			mkEvent(2, store.EventState, at(2), map[string]any{"status": store.RunRunning}),
			mkEvent(3, store.EventState, at(3), map[string]any{"status": store.RunRunning}),
			mkEvent(4, store.EventState, at(4), map[string]any{"status": store.RunRunning}),
		}},
	}
	entries, _ := newLedger(t, st, Sources{}).ForRun(context.Background(), "r", 2)
	if len(entries) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d", len(entries))
	}
	// 全量是 plan(at0) + 4 条 state(at1..4)，最后 2 条应是 at3、at4。
	if !entries[0].Time.Equal(at(3)) || !entries[1].Time.Equal(at(4)) {
		t.Errorf("应返回最新的 at3/at4，实际 %v/%v", entries[0].Time, entries[1].Time)
	}
}

// 终态若事件流里没有对应 run_state，要补一条 KindState；有的话不重复补。
func TestTerminalStateSynthesis(t *testing.T) {
	mk := func(withState bool) *fakeRunReader {
		evs := []store.Event{}
		if withState {
			evs = append(evs, mkEvent(1, store.EventState, at(2), map[string]any{"status": store.RunCompleted}))
		}
		return &fakeRunReader{
			runs: map[string]store.Run{
				"r": {ID: "r", SessionID: "s", Status: store.RunCompleted, Input: "x",
					CreatedAt: at(0), EndedAt: at(10)},
			},
			events: map[string][]store.Event{"r": evs},
		}
	}
	without, _ := newLedger(t, mk(false), Sources{}).ForRun(context.Background(), "r", 0)
	if countKind(without, KindState) != 1 {
		t.Fatalf("事件流无终态时应补 1 条 KindState，实际 %d", countKind(without, KindState))
	}
	with, _ := newLedger(t, mk(true), Sources{}).ForRun(context.Background(), "r", 0)
	if countKind(with, KindState) != 1 {
		t.Fatalf("事件流已有终态时不应重复补，实际 %d", countKind(with, KindState))
	}
}

// Summary 统计四类计数，Current 取「最新一条 blocked」的 Title。
func TestSummary(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r": {ID: "r", SessionID: "s", Status: store.RunRunning, Input: "x", CreatedAt: at(0)},
		},
		events: map[string][]store.Event{"r": {
			mkEvent(1, store.EventToolUpdate, at(1), map[string]any{"toolCallId": "t", "status": "completed"}),
			mkEvent(2, store.EventState, at(2), map[string]any{"status": store.RunWaitingApproval}),
			mkEvent(3, store.EventApprovalReq, at(3), map[string]any{
				"toolCallId": "tc", "title": "删库", "pending": true}), // 最新的一条 blocked
			mkEvent(4, store.EventError, at(4), map[string]any{"message": "x"}),
			mkEvent(5, store.EventToolStart, at(5), map[string]any{"toolCallId": "t2", "title": "跑"}),
		}},
	}
	s, err := newLedger(t, st, Sources{}).Summary(context.Background(), "r")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if s.Done == 0 || s.Running == 0 || s.Blocked != 2 || s.Failed != 1 {
		t.Errorf("计数不对: %+v", s)
	}
	if s.Current != "等待审批 · 删库" {
		t.Errorf("Current 应取最新一条 blocked 的 Title，实际 %q", s.Current)
	}
}

// 没有任何 blocked 项时 Current 为空串。
func TestSummaryNoBlocked(t *testing.T) {
	st := &fakeRunReader{runs: map[string]store.Run{
		"r": {ID: "r", SessionID: "s", Status: store.RunCompleted, Input: "x", CreatedAt: at(0)},
	}}
	s, _ := newLedger(t, st, Sources{}).Summary(context.Background(), "r")
	if s.Current != "" {
		t.Errorf("无阻塞项时 Current 应为空串，实际 %q", s.Current)
	}
}

// 任一来源报错都不该让账本报错：记日志并跳过该来源，其余照常返回。
func TestFaultTolerance(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r": {ID: "r", SessionID: "s", Status: store.RunRunning, Input: "计划", CreatedAt: at(0)},
		},
		evtErr: errors.New("事件流挂了"),
	}
	eg := &fakeEgress{err: errors.New("出网日志挂了")}
	kids := &fakeChildren{entries: []ChildEntry{
		{RunID: "c1", Title: "子", Status: "completed", EndedAt: at(2)},
	}}
	l := newLedger(t, st, Sources{Egress: eg, Children: kids})
	entries, err := l.ForRun(context.Background(), "r", 0)
	if err != nil {
		t.Fatalf("来源报错不该让账本返回错误: %v", err)
	}
	if countKind(entries, KindPlan) != 1 || countKind(entries, KindChild) != 1 {
		t.Errorf("事件流/出网挂了，但 plan 与 child 应照常返回: %+v", entries)
	}
}

// ForSession：合并多个 run 的 ForRun，按时间升序、截 limit。
func TestForSession(t *testing.T) {
	st := &fakeRunReader{
		runs: map[string]store.Run{
			"r1": {ID: "r1", SessionID: "s", Status: store.RunRunning, Input: "一", CreatedAt: at(0)},
			"r2": {ID: "r2", SessionID: "s", Status: store.RunRunning, Input: "二", CreatedAt: at(10)},
		},
		list: map[string][]store.Run{"s": {
			{ID: "r1", SessionID: "s", Status: store.RunRunning, CreatedAt: at(0)},
			{ID: "r2", SessionID: "s", Status: store.RunRunning, CreatedAt: at(10)},
		}},
	}
	l := newLedger(t, st, Sources{})
	entries, err := l.ForSession(context.Background(), "s", 0)
	if err != nil {
		t.Fatalf("ForSession: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应合并出 2 条 plan，实际 %d", len(entries))
	}
	if entries[0].RunID != "r1" || entries[1].RunID != "r2" {
		t.Errorf("合并后未按时间升序 / RunID 不对: %+v", entries)
	}
	// ListRuns 报错也不能让账本 503。
	st.listErr = errors.New("挂了")
	if got, err := l.ForSession(context.Background(), "s", 0); err != nil || len(got) != 0 {
		t.Errorf("ListRuns 报错应降级为空且无错误，实际 len=%d err=%v", len(got), err)
	}
}
