package sqlstore

// 测试跑在 t.TempDir() 下的 sqlite 文件上（不是 :memory:）：
// 内存库只有一个连接，看不出并发问题，也测不到「重开后 seq 继续」这条性质。
//
// PG 路径需要真库，本机没有，靠 ASTRA_TEST_PG 环境变量开启；
// 未设置时 TestPostgres 直接 Skip，不假装测过。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite://" + filepath.Join(t.TempDir(), "astra.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedSession 建一个带租户归属的会话 + 一个 run，返回两者的 id。
func seedSession(t *testing.T, s *Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	sid, rid := newID(), newID()
	if err := s.CreateSession(ctx, store.Session{
		ID: sid, TenantID: "t1", UserID: "u1", SandboxID: sid,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateRun(ctx, store.Run{ID: rid, SessionID: sid, Input: "hi"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return sid, rid
}

func TestOpenRejectsUnknownScheme(t *testing.T) {
	for _, dsn := range []string{"", "mysql://x", "/tmp/x.db", "http://x"} {
		if _, err := Open(dsn); err == nil {
			t.Errorf("DSN %q 应被拒绝", dsn)
		}
	}
}

func TestOpenAcceptsBothSQLitePrefixes(t *testing.T) {
	dir := t.TempDir()
	for _, dsn := range []string{
		"sqlite://" + filepath.Join(dir, "a.db"),
		"sqlite:" + filepath.Join(dir, "b.db"),
	} {
		s, err := Open(dsn)
		if err != nil {
			t.Fatalf("Open(%q): %v", dsn, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

// 一份语句两种方言：只有 PG 需要把 ? 换成 $n，且不能动字符串字面量里的 ?。
func TestRebind(t *testing.T) {
	sq := &Store{dialect: dialectSQLite}
	if got := sq.rebind("SELECT 1 WHERE a = ?"); got != "SELECT 1 WHERE a = ?" {
		t.Errorf("sqlite 不该改写占位符: %s", got)
	}

	pg := &Store{dialect: dialectPostgres}
	got := pg.rebind("SELECT a FROM t WHERE b = ? AND c = ? LIMIT ?")
	want := "SELECT a FROM t WHERE b = $1 AND c = $2 LIMIT $3"
	if got != want {
		t.Errorf("rebind = %q，期望 %q", got, want)
	}
	got = pg.rebind("SELECT '?' AS q, a FROM t WHERE b = ?")
	want = "SELECT '?' AS q, a FROM t WHERE b = $1"
	if got != want {
		t.Errorf("引号内的 ? 不该被替换: %q", got)
	}
}

// DDL 切分错了迁移会静默少建表，所以单独盯一下切分结果。
func TestSplitStatements(t *testing.T) {
	for name, script := range map[string]string{"sqlite": schemaSQLite, "pg": schemaPG} {
		stmts := splitStatements(script)
		if len(stmts) < 15 {
			t.Errorf("%s: 只切出 %d 条语句，明显偏少", name, len(stmts))
		}
		for _, stmt := range stmts {
			if !strings.HasPrefix(strings.ToUpper(stmt), "CREATE") {
				t.Errorf("%s: 切出了非 DDL 片段: %q", name, stmt)
			}
			if strings.Contains(stmt, "--") {
				t.Errorf("%s: 注释没剥干净: %q", name, stmt)
			}
		}
	}
}

// 幂等：同一个库文件可以重复 Open，且 seq 接着之前的走（不能重号）。
func TestMigrateIdempotentAndSeqSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astra.db")

	s1, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("首次 Open: %v", err)
	}
	_, rid := seedSession(t, s1)
	for i := 0; i < 3; i++ {
		if _, err := s1.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog, Payload: map[string]any{"i": i}}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer s2.Close()

	ev, err := s2.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog, Payload: map[string]any{"after": "reopen"}})
	if err != nil {
		t.Fatalf("重开后 Append: %v", err)
	}
	if ev.Seq != 4 {
		t.Fatalf("重开后 seq = %d，期望 4", ev.Seq)
	}
	all, err := s2.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("重开后共 %d 条，期望 4", len(all))
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created := time.Date(2026, 10, 30, 8, 0, 0, 123456789, time.UTC)
	in := store.Session{
		ID: "sess-1", TenantID: "t1", UserID: "u1", SandboxID: "sess-1",
		AgentSessionID: "acp-1", Title: "标题", Cwd: "/work", Status: store.SessionActive,
		CreatedAt: created, LastActiveAt: created,
	}
	if err := s.CreateSession(ctx, in); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.TenantID != "t1" || got.UserID != "u1" || got.Title != "标题" || got.Cwd != "/work" {
		t.Errorf("字段未往返: %+v", got)
	}
	if !got.CreatedAt.Equal(created) || got.CreatedAt.Location() != time.UTC {
		t.Errorf("时间未按 UTC 往返: %v (loc=%v)", got.CreatedAt, got.CreatedAt.Location())
	}

	// PutSession 是覆盖写，必须能改 title 而不丢其它字段
	got.Title = "改过的标题"
	got.LastActiveAt = created.Add(time.Hour)
	if err := s.PutSession(ctx, got); err != nil {
		t.Fatalf("PutSession: %v", err)
	}
	again, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if again.Title != "改过的标题" || !again.LastActiveAt.Equal(created.Add(time.Hour)) {
		t.Errorf("PutSession 未生效: %+v", again)
	}

	// PutSession 对不存在的会话是插入（upsert）。时间显式给，避免依赖「现在」
	// 与上面那个固定时间的先后关系。
	if err := s.PutSession(ctx, store.Session{
		ID: "sess-2", TenantID: "t1", CreatedAt: created.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("PutSession(new): %v", err)
	}
	list, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("共 %d 个会话，期望 2", len(list))
	}
	if list[0].ID != "sess-2" {
		t.Errorf("ListSessions 应按 created_at 倒序，实际第一条 %s", list[0].ID)
	}

	if err := s.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "sess-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("删除后应 ErrNotFound，实际 %v", err)
	}
	// 删不存在的会话与 file store 一致：静默成功
	if err := s.DeleteSession(ctx, "sess-1"); err != nil {
		t.Errorf("删不存在的会话不该报错: %v", err)
	}
}

func TestDuplicateSessionIsConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateSession(ctx, store.Session{ID: "dup"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateSession(ctx, store.Session{ID: "dup"}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("重复会话应 ErrConflict，实际 %v", err)
	}
}

func TestRunLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sid, rid := seedSession(t, s)

	r, err := s.GetRun(ctx, rid)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.SessionID != sid || r.Status != store.RunQueued || r.NextSeq != 1 {
		t.Errorf("默认值不符: %+v", r)
	}

	started := time.Now().UTC().Truncate(time.Microsecond)
	r.Status = store.RunRunning
	r.StartedAt = started
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	r2, err := s.GetRun(ctx, rid)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r2.Status != store.RunRunning || !r2.StartedAt.Equal(started) {
		t.Errorf("UpdateRun 未生效: %+v", r2)
	}

	// Append 会推进 next_seq，UpdateRun 不能把它写回去
	if _, err := s.Append(ctx, rid, store.NewEvent{Type: store.EventState}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	stale := r2 // 手里这份 NextSeq 还是旧值
	if err := s.UpdateRun(ctx, stale); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	r3, _ := s.GetRun(ctx, rid)
	if r3.NextSeq != 2 {
		t.Errorf("UpdateRun 不该回退 next_seq，实际 %d", r3.NextSeq)
	}

	list, err := s.ListRuns(ctx, sid)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(list) != 1 || list[0].ID != rid {
		t.Errorf("ListRuns 不符: %+v", list)
	}

	if err := s.UpdateRun(ctx, store.Run{ID: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("更新不存在的 run 应 ErrNotFound，实际 %v", err)
	}
	if _, err := s.GetRun(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("未知 run 应 ErrNotFound，实际 %v", err)
	}
}

// 删会话必须连带删掉 run 和事件：外键 ON DELETE CASCADE 生效的前提是
// SQLite 连接上真的开了 foreign_keys（默认是关的）。
func TestDeleteSessionCascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sid, rid := seedSession(t, s)
	if _, err := s.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.DeleteSession(ctx, sid); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetRun(ctx, rid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("删会话后 run 应不存在，实际 %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM run_events WHERE run_id = ?", rid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("删会话后事件应清空，实际 %d 条", n)
	}
}

func TestEventRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedSession(t, s)

	for i := 1; i <= 4; i++ {
		ev, err := s.Append(ctx, rid, store.NewEvent{
			Type:    store.EventMessageDelta,
			Payload: map[string]any{"i": i},
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if ev.Seq != int64(i) {
			t.Errorf("第 %d 条 seq = %d", i, ev.Seq)
		}
		if ev.RunID != rid || ev.Type != store.EventMessageDelta {
			t.Errorf("返回的事件字段不对: %+v", ev)
		}
		if ev.CreatedAt.IsZero() || ev.CreatedAt.Location() != time.UTC {
			t.Errorf("事件时间应是 UTC: %v", ev.CreatedAt)
		}
	}

	all, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("回放 %d 条，期望 4", len(all))
	}
	var p map[string]int
	if err := json.Unmarshal(all[0].Payload, &p); err != nil {
		t.Fatalf("payload 不是合法 JSON: %s", all[0].Payload)
	}
	if p["i"] != 1 {
		t.Errorf("payload 内容不符: %s", all[0].Payload)
	}

	after, err := s.Events(ctx, rid, 2, 0)
	if err != nil {
		t.Fatalf("Events(after=2): %v", err)
	}
	if len(after) != 2 || after[0].Seq != 3 {
		t.Errorf("after_seq=2 应返回 seq 3,4，实际 %+v", after)
	}

	limited, err := s.Events(ctx, rid, 0, 2)
	if err != nil {
		t.Fatalf("Events(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit=2 应返回 2 条，实际 %d", len(limited))
	}

	// 空 payload 统一落成 {}
	ev, err := s.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog})
	if err != nil {
		t.Fatalf("Append(nil payload): %v", err)
	}
	if string(ev.Payload) != "{}" {
		t.Errorf("空 payload 应为 {}，实际 %s", ev.Payload)
	}
}

func TestEventUnknownRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, "nope", store.NewEvent{Type: store.EventAgentLog}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("给不存在的 run 追加应 ErrNotFound，实际 %v", err)
	}
	if _, err := s.Events(ctx, "nope", 0, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("读不存在的 run 的事件应 ErrNotFound，实际 %v", err)
	}
}

// 并发追加是整个存储层最难的一块：seq 必须严格 1..N 不重号。
// 分配靠单条 UPDATE ... RETURNING 在数据库里串行化，进程内没有锁。
func TestConcurrentAppendNoDuplicateSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedSession(t, s)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	seqs := make(chan int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev, err := s.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog, Payload: map[string]any{"x": 1}})
			if err != nil {
				errs <- err
				return
			}
			seqs <- ev.Seq
		}()
	}
	wg.Wait()
	close(errs)
	close(seqs)

	for err := range errs {
		t.Fatalf("并发 Append 失败: %v", err)
	}

	seen := map[int64]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Fatalf("seq 重复: %d", seq)
		}
		seen[seq] = true
		if seq < 1 || seq > n {
			t.Fatalf("seq 越界: %d", seq)
		}
	}
	if len(seen) != n {
		t.Fatalf("只拿到 %d 个不同 seq，期望 %d", len(seen), n)
	}

	all, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(all) != n {
		t.Fatalf("落库 %d 条，期望 %d", len(all), n)
	}
	for i, ev := range all {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d，说明中间有空洞或乱序", i, ev.Seq)
		}
	}
}

// ── 治理层 ───────────────────────────────────────────────────

func TestTenantCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	t1 := store.Tenant{ID: "t1", Name: "一号", Slug: "one", MonthlyTokenLimit: 1000}
	if err := s.CreateTenant(ctx, t1); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	got, err := s.GetTenant(ctx, "t1")
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Slug != "one" || got.Status != store.StatusActive || got.MonthlyTokenLimit != 1000 {
		t.Errorf("租户默认值/字段不符: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at 应自动填 UTC: %v", got.CreatedAt)
	}

	bySlug, err := s.GetTenantBySlug(ctx, "one")
	if err != nil || bySlug.ID != "t1" {
		t.Fatalf("GetTenantBySlug: %+v %v", bySlug, err)
	}

	if err := s.CreateTenant(ctx, store.Tenant{ID: "t2", Slug: "one"}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("slug 重复应 ErrDuplicate，实际 %v", err)
	}

	got.Status = store.StatusSuspended
	got.MonthlyTokenLimit = 0
	if err := s.UpdateTenant(ctx, got); err != nil {
		t.Fatalf("UpdateTenant: %v", err)
	}
	after, _ := s.GetTenant(ctx, "t1")
	if after.Status != store.StatusSuspended {
		t.Errorf("UpdateTenant 未生效: %+v", after)
	}
	if err := s.UpdateTenant(ctx, store.Tenant{ID: "nope", Slug: "x"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("更新不存在的租户应 ErrNotFound，实际 %v", err)
	}

	list, err := s.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("共 %d 个租户，期望 1", len(list))
	}
}

func TestUserDuplicateEmail(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateTenant(ctx, store.Tenant{ID: "t1", Slug: "one"}); err != nil {
		t.Fatal(err)
	}

	u := store.User{ID: "u1", TenantID: "t1", Email: "alice@example.com", Role: store.RoleOwner}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// 大小写不同也算同一个邮箱：登录是大小写不敏感的
	dup := store.User{ID: "u2", TenantID: "t1", Email: "Alice@Example.com"}
	if err := s.CreateUser(ctx, dup); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("同租户邮箱重复应 ErrDuplicate，实际 %v", err)
	}
	// 另一个租户可以用同一邮箱
	if err := s.CreateTenant(ctx, store.Tenant{ID: "t2", Slug: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, store.User{ID: "u3", TenantID: "t2", Email: "alice@example.com"}); err != nil {
		t.Errorf("跨租户同邮箱应允许: %v", err)
	}
}

func TestUserCRUDAndCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateTenant(ctx, store.Tenant{ID: "t1", Slug: "one"}); err != nil {
		t.Fatal(err)
	}
	u := store.User{ID: "u1", TenantID: "t1", Email: "a@example.com", PasswordHash: "h", Role: store.RoleMember}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	got, err := s.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Status != store.StatusActive || got.PasswordHash != "h" || got.Role != store.RoleMember {
		t.Errorf("用户字段不符: %+v", got)
	}

	byEmail, err := s.GetUserByEmail(ctx, "t1", "A@Example.com")
	if err != nil || byEmail.ID != "u1" {
		t.Fatalf("GetUserByEmail 应大小写不敏感: %+v %v", byEmail, err)
	}
	if _, err := s.GetUserByEmail(ctx, "t2", "a@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("跨租户查邮箱应 ErrNotFound，实际 %v", err)
	}

	login := time.Now().UTC()
	got.LastLoginAt = login
	// 精度：SQLite 的落库格式是固定 9 位纳秒（见 sqliteTimeLayout），无损。
	// 原来断言拿 login.Truncate(time.Microsecond) 比，等于假设存储只到微秒——
	// 在能完整保留纳秒的环境下必然不等（CI 上就是这么红的）。
	// 这里改成容差 ≤1 微秒：既能抓住「根本没写进去」（差值是分钟级），
	// 又不依赖具体方言的精度。
	got.Role = store.RoleAdmin
	if err := s.UpdateUser(ctx, got); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	after, _ := s.GetUser(ctx, "u1")
	if after.Role != store.RoleAdmin {
		t.Errorf("UpdateUser 未生效（role）: %+v", after)
	}
	if diff := after.LastLoginAt.Sub(login); diff > time.Microsecond || diff < -time.Microsecond {
		t.Errorf("UpdateUser 未生效（last_login_at 偏差 %v）: %+v", diff, after)
	}

	n, err := s.CountUsers(ctx, "t1")
	if err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v", n, err)
	}
	if n, _ := s.CountUsers(ctx, ""); n != 1 {
		t.Errorf("空 tenantID 应统计全部，实际 %d", n)
	}
	if n, _ := s.CountUsers(ctx, "t2"); n != 0 {
		t.Errorf("t2 应 0 个用户，实际 %d", n)
	}

	if err := s.CreateUser(ctx, store.User{ID: "u9", TenantID: "t1", Email: "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUsers(ctx, "t1")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListUsers = %d, %v", len(list), err)
	}
	if all, _ := s.ListUsers(ctx, ""); len(all) != 2 {
		t.Errorf("空 tenantID 应列出全部用户，实际 %d", len(all))
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateTenant(ctx, store.Tenant{ID: "t1", Slug: "one"}); err != nil {
		t.Fatal(err)
	}

	k := store.APIKey{
		ID: "k1", TenantID: "t1", UserID: "u1", Name: "cli",
		Prefix: "astra_sk_a1b2c3", KeyHash: "hash-1",
	}
	if err := s.CreateAPIKey(ctx, k); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got, err := s.GetAPIKeyByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetAPIKeyByHash: %v", err)
	}
	if got.Prefix != "astra_sk_a1b2c3" || got.Revoked() {
		t.Errorf("key 字段不符: %+v", got)
	}
	if err := s.CreateAPIKey(ctx, store.APIKey{ID: "k2", TenantID: "t1", KeyHash: "hash-1"}); !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("hash 重复应 ErrDuplicate，实际 %v", err)
	}

	touched := time.Now().UTC()
	if err := s.TouchAPIKey(ctx, "k1", touched); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}
	got, _ = s.GetAPIKeyByHash(ctx, "hash-1")
	if got.LastUsedAt.IsZero() {
		t.Error("TouchAPIKey 未写入 last_used_at")
	}
	// 不存在的 key 只是旁路写失败，不能报错
	if err := s.TouchAPIKey(ctx, "nope", touched); err != nil {
		t.Errorf("TouchAPIKey 对未知 key 不该报错: %v", err)
	}

	if err := s.RevokeAPIKey(ctx, "t1", "k1"); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	got, _ = s.GetAPIKeyByHash(ctx, "hash-1")
	if !got.Revoked() {
		t.Error("撤销后 Revoked() 应为 true")
	}
	// 跨租户撤销必须查无此 key
	if err := s.RevokeAPIKey(ctx, "t2", "k1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("跨租户撤销应 ErrNotFound，实际 %v", err)
	}

	list, err := s.ListAPIKeys(ctx, "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAPIKeys = %d, %v", len(list), err)
	}
	if all, _ := s.ListAPIKeys(ctx, ""); len(all) != 1 {
		t.Errorf("空 tenantID 应列出全部 key，实际 %d", len(all))
	}
	if other, _ := s.ListAPIKeys(ctx, "t2"); len(other) != 0 {
		t.Errorf("t2 不该有 key，实际 %d", len(other))
	}
}

func TestUsageListAndSum(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	rows := []store.Usage{
		{ID: "x1", TenantID: "t1", UserID: "u1", SessionID: "s1", RunID: "r1",
			Provider: "deepseek", Model: "chat", InputTokens: 100, OutputTokens: 50,
			CostMicroUSD: 10, Source: "gateway", CreatedAt: base},
		{ID: "x2", TenantID: "t1", UserID: "u2", SessionID: "s1", RunID: "r2",
			Provider: "deepseek", Model: "chat", InputTokens: 200, OutputTokens: 100,
			CostMicroUSD: 20, Source: "gateway", CreatedAt: base.Add(time.Hour)},
		{ID: "x3", TenantID: "t2", UserID: "u3", SessionID: "s2", RunID: "r3",
			Provider: "openai", Model: "gpt", InputTokens: 7, OutputTokens: 3,
			CostMicroUSD: 1, Source: "agent", CreatedAt: base.Add(2 * time.Hour)},
	}
	for _, u := range rows {
		if err := s.InsertUsage(ctx, u); err != nil {
			t.Fatalf("InsertUsage: %v", err)
		}
	}
	// 不填 id / created_at 也要能落库（高频记账路径）
	if err := s.InsertUsage(ctx, store.Usage{TenantID: "t1", InputTokens: 1, Source: "gateway"}); err != nil {
		t.Fatalf("InsertUsage(自动 id): %v", err)
	}

	sum, err := s.SumUsage(ctx, store.UsageFilter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("SumUsage: %v", err)
	}
	if sum.Calls != 3 || sum.InputTokens != 301 || sum.OutputTokens != 150 || sum.CostMicroUSD != 30 {
		t.Errorf("t1 汇总不符: %+v", sum)
	}

	all, err := s.SumUsage(ctx, store.UsageFilter{})
	if err != nil || all.Calls != 4 {
		t.Fatalf("全量汇总 = %+v, %v", all, err)
	}

	byUser, _ := s.SumUsage(ctx, store.UsageFilter{TenantID: "t1", UserID: "u2"})
	if byUser.Calls != 1 || byUser.InputTokens != 200 {
		t.Errorf("按用户汇总不符: %+v", byUser)
	}
	bySession, _ := s.SumUsage(ctx, store.UsageFilter{SessionID: "s2"})
	if bySession.Calls != 1 || bySession.CostMicroUSD != 1 {
		t.Errorf("按会话汇总不符: %+v", bySession)
	}
	// 时间窗：To 是开区间
	window, _ := s.SumUsage(ctx, store.UsageFilter{From: base.Add(30 * time.Minute), To: base.Add(2 * time.Hour)})
	if window.Calls != 1 || window.InputTokens != 200 {
		t.Errorf("时间窗汇总不符: %+v", window)
	}

	list, err := s.ListUsage(ctx, store.UsageFilter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("ListUsage: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("t1 共 %d 条用量，期望 3", len(list))
	}
	if list[0].CreatedAt.Before(list[len(list)-1].CreatedAt) {
		t.Error("ListUsage 应按时间倒序")
	}

	limited, err := s.ListUsage(ctx, store.UsageFilter{TenantID: "t1", Limit: 2})
	if err != nil || len(limited) != 2 {
		t.Fatalf("Limit=2 应返回 2 条，实际 %d (%v)", len(limited), err)
	}
	// 负值表示不限
	unlimited, _ := s.ListUsage(ctx, store.UsageFilter{Limit: -1})
	if len(unlimited) != 4 {
		t.Errorf("Limit=-1 应返回全部 4 条，实际 %d", len(unlimited))
	}
}

func TestAuditFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	entries := []store.AuditLog{
		{ID: "a1", TenantID: "t1", UserID: "u1", ActorEmail: "u1@x.com", Action: "session.create",
			TargetType: "session", TargetID: "s1", Detail: json.RawMessage(`{"title":"x"}`), IP: "1.1.1.1", CreatedAt: base},
		{ID: "a2", TenantID: "t1", UserID: "u2", Action: "apikey.revoke", TargetType: "apikey", TargetID: "k1", CreatedAt: base.Add(time.Hour)},
		{ID: "a3", TenantID: "t2", UserID: "u3", Action: "session.create", TargetType: "session", TargetID: "s2", CreatedAt: base.Add(2 * time.Hour)},
	}
	for _, a := range entries {
		if err := s.InsertAudit(ctx, a); err != nil {
			t.Fatalf("InsertAudit: %v", err)
		}
	}

	byTenant, err := s.ListAudit(ctx, store.AuditFilter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(byTenant) != 2 {
		t.Fatalf("t1 共 %d 条审计，期望 2", len(byTenant))
	}
	if byTenant[0].ID != "a2" {
		t.Errorf("应按时间倒序，第一条 %s", byTenant[0].ID)
	}
	if len(byTenant[1].Detail) == 0 || string(byTenant[1].Detail) != `{"title":"x"}` {
		t.Errorf("detail 未往返: %s", byTenant[1].Detail)
	}
	if byTenant[1].IP != "1.1.1.1" || byTenant[1].ActorEmail != "u1@x.com" {
		t.Errorf("审计字段未往返: %+v", byTenant[1])
	}

	byAction, _ := s.ListAudit(ctx, store.AuditFilter{Action: "session.create"})
	if len(byAction) != 2 {
		t.Errorf("按 action 过滤应 2 条，实际 %d", len(byAction))
	}
	byUser, _ := s.ListAudit(ctx, store.AuditFilter{TenantID: "t1", UserID: "u2"})
	if len(byUser) != 1 || byUser[0].ID != "a2" {
		t.Errorf("按 user 过滤不符: %+v", byUser)
	}
	byTime, _ := s.ListAudit(ctx, store.AuditFilter{From: base.Add(90 * time.Minute)})
	if len(byTime) != 1 || byTime[0].ID != "a3" {
		t.Errorf("按时间过滤不符: %+v", byTime)
	}
	limited, _ := s.ListAudit(ctx, store.AuditFilter{Limit: 1})
	if len(limited) != 1 {
		t.Errorf("Limit=1 应返回 1 条，实际 %d", len(limited))
	}
	// 空 detail 落 NULL，读回来是 nil
	if err := s.InsertAudit(ctx, store.AuditLog{ID: "a4", TenantID: "t1", Action: "logout"}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	got, _ := s.ListAudit(ctx, store.AuditFilter{TenantID: "t1", Action: "logout"})
	if len(got) != 1 || len(got[0].Detail) != 0 {
		t.Errorf("空 detail 应为空: %+v", got)
	}
}

func TestListSessionsByTenant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	create := func(id, tenant, user string, active time.Time) {
		t.Helper()
		if err := s.CreateSession(ctx, store.Session{
			ID: id, TenantID: tenant, UserID: user, LastActiveAt: active,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	create("s1", "t1", "u1", now.Add(-time.Hour))
	create("s2", "t1", "u2", now)
	create("s3", "t2", "u3", now)

	got, err := s.ListSessionsByTenant(ctx, "t1")
	if err != nil {
		t.Fatalf("ListSessionsByTenant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("t1 共 %d 个会话，期望 2", len(got))
	}
	if got[0].ID != "s2" {
		t.Errorf("应按 last_active_at 倒序，第一条 %s", got[0].ID)
	}
	if got[1].TenantID != "t1" || got[1].UserID != "u1" {
		t.Errorf("归属字段未往返: %+v", got[1])
	}

	// 空 tenantID = 跨租户（平台超管）
	all, err := s.ListSessionsByTenant(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("空 tenantID 应返回全部 3 个，实际 %d (%v)", len(all), err)
	}
	if none, _ := s.ListSessionsByTenant(ctx, "t3"); len(none) != 0 {
		t.Errorf("t3 不该有会话，实际 %d", len(none))
	}
}

// 本地时区写进去，读出来必须是 UTC 同一瞬间——否则前端显示会飘 8 小时。
func TestTimesNormalizedToUTC(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	cst := time.FixedZone("CST", 8*3600)
	local := time.Date(2026, 10, 30, 20, 30, 0, 987654321, cst)
	if err := s.CreateSession(ctx, store.Session{ID: "s1", CreatedAt: local}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.CreatedAt.Equal(local) {
		t.Errorf("时间点变了: %v != %v", got.CreatedAt, local)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("读出来应是 UTC，实际 %v", got.CreatedAt.Location())
	}
	if h := got.CreatedAt.Hour(); h != 12 { // 20:30 +08:00 = 12:30 UTC
		t.Errorf("UTC 小时 = %d，期望 12", h)
	}
}

// PG 路径本机无库，靠 ASTRA_TEST_PG 开启；未设置时跳过而不是假装通过。
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("ASTRA_TEST_PG")
	if dsn == "" {
		t.Skip("ASTRA_TEST_PG 未设置，跳过 PG 路径")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open(PG): %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// 每次运行用随机后缀，避免与库里的残留数据撞唯一约束
	suffix := newID()
	tenantID, userID := "t-"+suffix, "u-"+suffix
	if err := s.CreateTenant(ctx, store.Tenant{ID: tenantID, Name: "PG 测试", Slug: "pg-" + suffix}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.CreateUser(ctx, store.User{ID: userID, TenantID: tenantID, Email: "a@example.com", Role: store.RoleOwner}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateUser(ctx, store.User{ID: userID + "-dup", TenantID: tenantID, Email: "A@example.com"}); !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("PG 上邮箱重复应 ErrDuplicate，实际 %v", err)
	}

	sid, rid := "s-"+suffix, "r-"+suffix
	if err := s.CreateSession(ctx, store.Session{ID: sid, TenantID: tenantID, UserID: userID}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateRun(ctx, store.Run{ID: rid, SessionID: sid, Input: "hi"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteSession(context.Background(), sid) })

	// 并发 Append 在 PG 上靠行锁串行化，这里同样抽查 20 次
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Append(ctx, rid, store.NewEvent{Type: store.EventAgentLog}); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()

	evs, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 20 {
		t.Fatalf("PG 上落库 %d 条，期望 20", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq = %d", i, ev.Seq)
		}
		if ev.CreatedAt.Location() != time.UTC {
			t.Fatalf("PG 时间应读出 UTC，实际 %v", ev.CreatedAt.Location())
		}
	}

	if err := s.InsertUsage(ctx, store.Usage{TenantID: tenantID, InputTokens: 10, OutputTokens: 5, CostMicroUSD: 1, Source: "gateway"}); err != nil {
		t.Fatalf("InsertUsage: %v", err)
	}
	sum, err := s.SumUsage(ctx, store.UsageFilter{TenantID: tenantID})
	if err != nil || sum.Calls != 1 || sum.InputTokens != 10 {
		t.Fatalf("PG 汇总不符: %+v %v", sum, err)
	}
	if err := s.InsertAudit(ctx, store.AuditLog{TenantID: tenantID, Action: "session.create", Detail: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatalf("InsertAudit: %v", err)
	}
	audits, err := s.ListAudit(ctx, store.AuditFilter{TenantID: tenantID})
	if err != nil || len(audits) != 1 || string(audits[0].Detail) != `{"a":1}` {
		t.Fatalf("PG 审计不符: %+v %v", audits, err)
	}
	tenants, err := s.ListSessionsByTenant(ctx, tenantID)
	if err != nil || len(tenants) != 1 {
		t.Fatalf("PG 按租户列会话不符: %+v %v", tenants, err)
	}
}

// 终态不可逆（sqlstore 版）：非终态的写入不能覆盖已落库的终态。
// 与 file store 的 TestUpdateRunTerminalIsSticky 同一语义——生产走 PG，
// 少了这条保护，取消/失败后状态会被审批回调的旧快照顶回 running。
func TestUpdateRunTerminalIsSticky(t *testing.T) {
	ctx := context.Background()
	s, err := Open("sqlite://" + filepath.Join(t.TempDir(), "sticky.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	_, rid := seedSession(t, s)
	r, err := s.GetRun(ctx, rid)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	r.Status = store.RunCancelled
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("写终态: %v", err)
	}

	stale := r
	stale.Status = store.RunRunning
	if err := s.UpdateRun(ctx, stale); err != nil {
		t.Fatalf("旧快照回写: %v", err)
	}
	got, err := s.GetRun(ctx, rid)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != store.RunCancelled {
		t.Fatalf("终态被覆盖：want %s, got %s", store.RunCancelled, got.Status)
	}

	// 不存在的 run 仍然要报 ErrNotFound，不能因为「条件更新没命中」就静默成功
	if err := s.UpdateRun(ctx, store.Run{ID: "nope", Status: store.RunRunning}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未知 run 应 ErrNotFound，实际 %v", err)
	}
}
