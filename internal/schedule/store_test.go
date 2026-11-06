package schedule

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, max int) (*fileStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := openFileStore(dir, max)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	return st, dir
}

func sampleCron(tenant string) Schedule {
	return Schedule{
		TenantID:  tenant,
		UserID:    "u1",
		SessionID: "s1",
		Name:      "日报",
		Trigger:   TriggerCron,
		Cron:      "*/5 * * * *",
		Prompt:    "整理今天的工单",
		Enabled:   true,
	}
}

func TestStoreCreateCronDefaults(t *testing.T) {
	st, dir := newTestStore(t, 0)
	ctx := context.Background()

	got, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ID) != 16 {
		t.Fatalf("ID 应为 16 位 hex，实际 %q", got.ID)
	}
	if _, err := hex.DecodeString(got.ID); err != nil {
		t.Fatalf("ID 不是 hex: %v", err)
	}
	if got.HookToken != "" {
		t.Fatalf("cron 型不该有 hook token: %q", got.HookToken)
	}
	if got.NextRunAt.IsZero() {
		t.Fatal("启用的 cron 型必须有 NextRunAt")
	}
	if !got.NextRunAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("NextRunAt 应该在未来，实际 %s", got.NextRunAt)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("时间戳缺失")
	}

	// 落盘了才叫持久化
	b, err := os.ReadFile(filepath.Join(dir, "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var df diskFile
	if err := json.Unmarshal(b, &df); err != nil {
		t.Fatalf("文件不是合法 JSON: %v", err)
	}
	if len(df.Items) != 1 || df.Items[0].ID != got.ID {
		t.Fatalf("落盘内容不对: %+v", df)
	}
	info, err := os.Stat(filepath.Join(dir, "schedules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限应为 0600，实际 %o", perm)
	}

	// 重新打开（模拟重启）能读回
	st2, err := openFileStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	back, err := st2.Get(ctx, got.ID)
	if err != nil || !back.NextRunAt.Equal(got.NextRunAt) {
		t.Fatalf("重启后读回不一致: %+v / %v", back, err)
	}
}

func TestStoreCreateWebhookToken(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()
	in := sampleCron("t1")
	in.Trigger = TriggerWebhook
	in.Cron = ""

	got, err := st.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.HookToken == "" {
		t.Fatal("webhook 型必须有 hook token")
	}
	// 32 字节 base64url 无填充 = 43 字符
	if len(got.HookToken) != 43 {
		t.Fatalf("token 长度应为 43，实际 %d（%q）", len(got.HookToken), got.HookToken)
	}
	if !got.NextRunAt.IsZero() {
		t.Fatalf("webhook 型不该有 NextRunAt: %s", got.NextRunAt)
	}
	if got2, err := st.Create(ctx, func() Schedule { x := in; x.Name = "第二个"; return x }()); err != nil {
		t.Fatal(err)
	} else if got2.HookToken == got.HookToken {
		t.Fatal("两条 webhook 不能拿到同一个 token")
	}

	byToken, err := st.GetByHookToken(ctx, got.HookToken)
	if err != nil || byToken.ID != got.ID {
		t.Fatalf("按 token 查不到: %+v / %v", byToken, err)
	}
	if _, err := st.GetByHookToken(ctx, "不存在的token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 token 应 ErrNotFound，实际 %v", err)
	}
	if _, err := st.GetByHookToken(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空 token 应 ErrNotFound，实际 %v", err)
	}
}

func TestStoreCreateInvalidInput(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()

	bad := sampleCron("t1")
	bad.Cron = "*/0 * * * *"
	if _, err := st.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法 cron 应 ErrInvalid，实际 %v", err)
	}
	bad = sampleCron("")
	if _, err := st.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("缺 tenant 应 ErrInvalid，实际 %v", err)
	}
	bad = sampleCron("t1")
	bad.Prompt = ""
	if _, err := st.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("缺 prompt 应 ErrInvalid，实际 %v", err)
	}
	bad = sampleCron("t1")
	bad.Trigger = "every"
	if _, err := st.Create(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未知 trigger 应 ErrInvalid，实际 %v", err)
	}
}

func TestStoreQuotaPerTenant(t *testing.T) {
	st, _ := newTestStore(t, 2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := st.Create(ctx, sampleCron("t1")); err != nil {
			t.Fatalf("第 %d 条不该失败: %v", i+1, err)
		}
	}
	_, err := st.Create(ctx, sampleCron("t1"))
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("超出配额应 ErrQuota，实际 %v", err)
	}
	// 别的租户不受影响
	if _, err := st.Create(ctx, sampleCron("t2")); err != nil {
		t.Fatalf("另一个租户不该被挡: %v", err)
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	st, dir := newTestStore(t, 0)
	ctx := context.Background()

	const n = 30
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := sampleCron("t1")
			s.Name = fmt.Sprintf("job-%d", i)
			if _, err := st.Create(ctx, s); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发创建失败: %v", err)
	}

	items, err := st.List(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("期望 %d 条，实际 %d（有写丢失）", n, len(items))
	}
	seen := make(map[string]bool, n)
	for _, it := range items {
		if seen[it.ID] {
			t.Fatalf("ID 重复: %s", it.ID)
		}
		seen[it.ID] = true
	}
	assertFileValid(t, dir)
}

// 两个 store 实例 = 两个独立进程的近似：进程内 mutex 管不到对方，只能靠 flock。
func TestStoreCrossInstanceLock(t *testing.T) {
	dir := t.TempDir()
	a, err := openFileStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := openFileStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	const per = 10
	var wg sync.WaitGroup
	errs := make(chan error, 2*per)
	run := func(st *fileStore, tag string) {
		defer wg.Done()
		for i := 0; i < per; i++ {
			s := sampleCron("t1")
			s.Name = fmt.Sprintf("%s-%d", tag, i)
			if _, err := st.Create(ctx, s); err != nil {
				errs <- err
			}
		}
	}
	wg.Add(2)
	go run(a, "a")
	go run(b, "b")
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("跨实例并发创建失败: %v", err)
	}

	items, err := a.List(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2*per {
		t.Fatalf("期望 %d 条，实际 %d（跨进程锁没起作用，写丢了）", 2*per, len(items))
	}
	assertFileValid(t, dir)

	// 一方删除，另一方立刻看到（证明读的是同一份文件）
	if err := a.Delete(ctx, "t1", items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("另一个实例应立刻看不到已删除条目，实际 %v", err)
	}
}

func TestStoreDueFiltersAndOrder(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	mk := func(name string, next time.Time, enabled bool) Schedule {
		s := sampleCron("t1")
		s.Name = name
		s.Enabled = enabled
		out, err := st.Create(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.mutate(ctx, out.ID, func(cur *Schedule) error {
			cur.NextRunAt = next
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// 回读：Create 返回的是改之前的值
		fresh, err := st.Get(ctx, out.ID)
		if err != nil {
			t.Fatal(err)
		}
		return fresh
	}

	late := mk("late", now.Add(-2*time.Hour), true)
	early := mk("early", now.Add(-3*time.Hour), true)
	mk("future", now.Add(time.Hour), true)
	mk("disabled", now.Add(-time.Hour), false)
	mk("zero", time.Time{}, true) // 还没算出来（webhook 型常见）

	due, err := st.Due(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("期望 2 条到期，实际 %d: %+v", len(due), due)
	}
	if due[0].ID != early.ID || due[1].ID != late.ID {
		t.Fatalf("应按 next_run_at 升序，实际 %s, %s", due[0].Name, due[1].Name)
	}
	if limited, _ := st.Due(ctx, now, 1); len(limited) != 1 {
		t.Fatalf("limit 没生效，实际 %d 条", len(limited))
	}
	// 边界：正好等于 now 也算到期
	due2, _ := st.Due(ctx, early.NextRunAt, 10)
	if len(due2) != 1 || due2[0].ID != early.ID {
		t.Fatalf("NextRunAt == now 应算到期，实际 %+v", due2)
	}
}

func TestStoreUpdateRecomputesNextRunAt(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()

	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟调度器推进过 NextRunAt：Update 不该把它冲掉
	advanced := created.NextRunAt.Add(90 * time.Minute)
	if _, err := st.mutate(ctx, created.ID, func(cur *Schedule) error {
		cur.NextRunAt = advanced
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cur, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Name = "改名"
	if err := st.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	back, _ := st.Get(ctx, created.ID)
	if !back.NextRunAt.Equal(advanced) {
		t.Fatalf("只改名字不该重算 NextRunAt：%s → %s", advanced, back.NextRunAt)
	}
	if back.Name != "改名" {
		t.Fatalf("改名没生效: %q", back.Name)
	}

	// 改表达式 → 从 now 重算
	back.Cron = "0 9 * * *"
	if err := st.Update(ctx, back); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(ctx, created.ID)
	if after.NextRunAt.Equal(advanced) {
		t.Fatal("改 cron 后必须重算 NextRunAt")
	}
	if after.NextRunAt.Hour() != 9 {
		t.Fatalf("重算结果应落在 9 点，实际 %s", after.NextRunAt)
	}

	// 停用 → 清空
	after.Enabled = false
	if err := st.Update(ctx, after); err != nil {
		t.Fatal(err)
	}
	off, _ := st.Get(ctx, created.ID)
	if !off.NextRunAt.IsZero() {
		t.Fatalf("停用后 NextRunAt 应为零，实际 %s", off.NextRunAt)
	}

	// 非法 cron 要拒绝，且不能把已有数据改坏
	off.Enabled = true
	off.Cron = "99 * * * *"
	if err := st.Update(ctx, off); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法 cron 的 Update 应 ErrInvalid，实际 %v", err)
	}
	still, _ := st.Get(ctx, created.ID)
	if still.Cron != "0 9 * * *" {
		t.Fatalf("失败的 Update 不该改坏数据，实际 cron=%q", still.Cron)
	}
}

func TestStoreDeleteTenantIsolation(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()
	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "t2", created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户删除应报不存在，实际 %v", err)
	}
	if _, err := st.Get(ctx, created.ID); err != nil {
		t.Fatalf("跨租户删除不该真的删掉: %v", err)
	}
	if err := st.Delete(ctx, "t1", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应查不到，实际 %v", err)
	}
	if err := st.Update(ctx, created); !errors.Is(err, ErrNotFound) {
		t.Fatalf("更新已删除条目应 ErrNotFound，实际 %v", err)
	}
}

func TestStoreRefusesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schedules.json"), []byte("{不是 json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openFileStore(dir, 0); err == nil {
		t.Fatal("损坏的文件必须在启动时报错，而不是静默清空")
	}
	// 报错也不能把原文件踩掉
	b, err := os.ReadFile(filepath.Join(dir, "schedules.json"))
	if err != nil || string(b) != "{不是 json" {
		t.Fatalf("原文件被改动了: %q / %v", b, err)
	}
}

func TestStoreMissingFileIsEmpty(t *testing.T) {
	st, dir := newTestStore(t, 0)
	items, err := st.List(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("空目录应返回空列表，实际 %d", len(items))
	}
	if _, err := os.Stat(filepath.Join(dir, "schedules.json")); !os.IsNotExist(err) {
		t.Fatalf("只读操作不该创建文件: %v", err)
	}
}

func TestStoreClaimIsCompareAndSwap(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx := context.Background()
	created, err := st.Create(ctx, sampleCron("t1"))
	if err != nil {
		t.Fatal(err)
	}
	old := created.NextRunAt
	next := old.Add(5 * time.Minute)

	ok, err := st.claim(ctx, created.ID, old, next)
	if err != nil || !ok {
		t.Fatalf("第一次 claim 应成功: %v / %v", ok, err)
	}
	// 拿旧值再抢必须失败（这就是防重复触发的根据）
	ok, err = st.claim(ctx, created.ID, old, next.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("同一个 old 值不该被抢到两次")
	}
	back, _ := st.Get(ctx, created.ID)
	if !back.NextRunAt.Equal(next) {
		t.Fatalf("失败的 claim 不该改数据：want %s got %s", next, back.NextRunAt)
	}
}

func TestStoreContextCancelled(t *testing.T) {
	st, _ := newTestStore(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.List(ctx, "t1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应直接返回，实际 %v", err)
	}
}

func assertFileValid(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "schedules.json"))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var df diskFile
	if err := json.Unmarshal(b, &df); err != nil {
		t.Fatalf("并发写之后文件不是合法 JSON（写坏了）: %v", err)
	}
}
