package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var testBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func mkDiag(runID, sessionID string, at time.Time) Diagnosis {
	return Diagnosis{
		RunID: runID, SessionID: sessionID, Status: "failed",
		Error: "boom", Suggestion: "fix", CreatedAt: at,
	}
}

func newStore(t *testing.T) (Store, *fileStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, s.(*fileStore), filepath.Join(dir, insightsFileName)
}

// seedItems 批量预置缓存并一次落盘，避免几百次 fsync 拖慢排序/limit 测试。
// FIFO 截断逻辑在 append 里，由单独的用例显式触发。
func seedItems(t *testing.T, fs *fileStore, items []Diagnosis) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.items = items
	if err := writeInsights(fs.path, items); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOpenMissingFileIsOK(t *testing.T) {
	s, _, path := newStore(t)
	got, err := s.All(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("首启应为空, got %+v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("首启不该创建 insights.json: %v", err)
	}
}

func TestStoreOpenLoadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, insightsFileName)
	seed := []Diagnosis{
		mkDiag("r1", "s1", testBase),
		mkDiag("r2", "s1", testBase.Add(time.Hour)),
	}
	b, _ := json.Marshal(seed)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.All(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("条数 = %d, want 2", len(got))
	}
	if got[0].RunID != "r2" || got[1].RunID != "r1" {
		t.Fatalf("未按 CreatedAt 倒序: %q, %q", got[0].RunID, got[1].RunID)
	}
	if got[0].Error != "boom" || got[0].Suggestion != "fix" {
		t.Fatalf("字段没读全: %+v", got[0])
	}
}

func TestStoreOpenCorruptJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, insightsFileName)
	bad := []byte("{not json")
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("坏 JSON 必须报错，不能静默清空")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bad) {
		t.Fatalf("坏文件被改动了：%q", after)
	}
}

func TestStoreRoundTripAndFileMode(t *testing.T) {
	s, fs, path := newStore(t)
	d := mkDiag("r1", "s1", testBase)
	d.Error = "command not found: foo"
	d.LastTool = "bash"
	d.ToolCount = 3
	d.Blocked = 1
	d.Failed = 2
	if err := fs.append(d); err != nil {
		t.Fatal(err)
	}

	got, err := s.ForRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got != d {
		t.Fatalf("往返不一致:\n got=%+v\nwant=%+v", got, d)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("文件权限 = %o, want 600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []Diagnosis
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if len(onDisk) != 1 || onDisk[0].RunID != "r1" {
		t.Fatalf("落盘内容错误: %+v", onDisk)
	}
}

func TestStoreFIFOCap(t *testing.T) {
	s, fs, path := newStore(t)
	items := make([]Diagnosis, 0, maxItems)
	for i := 0; i < maxItems; i++ {
		items = append(items, mkDiag(fmt.Sprintf("r-%03d", i), "s1",
			testBase.Add(time.Duration(i)*time.Second)))
	}
	seedItems(t, fs, items)

	// 第 501 条触发截断：r-000（最旧）应被丢掉。
	last := mkDiag("r-500", "s1", testBase.Add(time.Duration(maxItems)*time.Second))
	if err := fs.append(last); err != nil {
		t.Fatal(err)
	}

	got, err := s.All(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxItems {
		t.Fatalf("条数 = %d, want %d", len(got), maxItems)
	}
	if got[0].RunID != "r-500" {
		t.Fatalf("最新一条应为 r-500, got %q", got[0].RunID)
	}
	if got[len(got)-1].RunID != "r-001" {
		t.Fatalf("最旧保留应为 r-001（r-000 被丢）, got %q", got[len(got)-1].RunID)
	}
	for _, d := range got {
		if d.RunID == "r-000" {
			t.Fatal("最旧记录没被 FIFO 淘汰")
		}
	}
	b, _ := os.ReadFile(path)
	var onDisk []Diagnosis
	if err := json.Unmarshal(b, &onDisk); err != nil || len(onDisk) != maxItems {
		t.Fatalf("落盘条数/JSON 异常: len=%d err=%v", len(onDisk), err)
	}
}

func TestStoreRecentFilterOrderDefaultLimit(t *testing.T) {
	s, fs, _ := newStore(t)
	var items []Diagnosis
	for i := 0; i < 25; i++ {
		at := testBase.Add(time.Duration(i) * time.Minute)
		items = append(items, mkDiag(fmt.Sprintf("s1-%02d", i), "s1", at))
		if i%5 == 0 {
			items = append(items, mkDiag(fmt.Sprintf("s2-%02d", i), "s2", at))
		}
	}
	seedItems(t, fs, items)

	got, err := s.Recent(context.Background(), "s1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 25 {
		t.Fatalf("过滤后条数 = %d, want 25", len(got))
	}
	for _, d := range got {
		if d.SessionID != "s1" {
			t.Fatalf("混入了别的会话: %+v", d)
		}
	}
	if got[0].RunID != "s1-24" || got[len(got)-1].RunID != "s1-00" {
		t.Fatalf("未按 CreatedAt 倒序: 首=%q 尾=%q", got[0].RunID, got[len(got)-1].RunID)
	}

	got2, _ := s.Recent(context.Background(), "s1", 3)
	if len(got2) != 3 || got2[0].RunID != "s1-24" {
		t.Fatalf("limit=3 错误: %+v", got2)
	}
	if got3, _ := s.Recent(context.Background(), "s1", 0); len(got3) != defaultRecentLimit {
		t.Fatalf("limit<=0 默认应 %d 条, got %d", defaultRecentLimit, len(got3))
	}
	if got4, _ := s.Recent(context.Background(), "s1", -1); len(got4) != defaultRecentLimit {
		t.Fatalf("limit=-1 默认应 %d 条, got %d", defaultRecentLimit, len(got4))
	}
}

func TestStoreAllDefaultLimit(t *testing.T) {
	s, fs, _ := newStore(t)
	items := make([]Diagnosis, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, mkDiag(fmt.Sprintf("r-%02d", i), "s1",
			testBase.Add(time.Duration(i)*time.Second)))
	}
	seedItems(t, fs, items)

	if got, _ := s.All(context.Background(), 0); len(got) != defaultAllLimit {
		t.Fatalf("limit<=0 默认应 %d 条, got %d", defaultAllLimit, len(got))
	}
	if got, _ := s.All(context.Background(), -5); len(got) != defaultAllLimit {
		t.Fatalf("limit=-5 默认应 %d 条, got %d", defaultAllLimit, len(got))
	}
	if got, _ := s.All(context.Background(), 1000); len(got) != 60 {
		t.Fatalf("limit=1000 应 60 条, got %d", len(got))
	}
	got, _ := s.All(context.Background(), 2)
	if len(got) != 2 || got[0].RunID != "r-59" {
		t.Fatalf("limit=2 或倒序错误: %+v", got)
	}
}

// ByTenant 只返回该租户的诊断；无归属老数据（接入认证前写入）对任何
// 非空租户都不可见——这是「猜不出归属就不展示」的取舍。
func TestStoreByTenantFiltersAndHidesLegacy(t *testing.T) {
	s, fs, _ := newStore(t)
	mk := func(runID, tid string, at time.Time) Diagnosis {
		d := mkDiag(runID, "s-"+tid, at)
		d.TenantID = tid
		d.UserID = "u-" + tid
		return d
	}
	items := []Diagnosis{
		mk("a1", "t1", testBase.Add(time.Minute)),
		mk("b1", "t2", testBase.Add(2*time.Minute)),
		mk("a2", "t1", testBase.Add(3*time.Minute)),
		mkDiag("old", "s-old", testBase.Add(4*time.Minute)), // TenantID 为空
	}
	seedItems(t, fs, items)

	got, err := s.ByTenant(context.Background(), "t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != "a2" || got[1].RunID != "a1" {
		t.Fatalf("t1 结果或排序错误: %+v", got)
	}
	for _, d := range got {
		if d.TenantID != "t1" || d.UserID != "u-t1" {
			t.Fatalf("归属字段丢失: %+v", d)
		}
	}

	// 空租户只会匹配空租户：正常身份（TenantID 非空）永远不会命中老数据。
	got, _ = s.ByTenant(context.Background(), "", 0)
	if len(got) != 1 || got[0].RunID != "old" {
		t.Fatalf("空租户应只返回无归属老数据: %+v", got)
	}

	// limit<=0 默认 50，limit 生效。
	if got, _ := s.ByTenant(context.Background(), "t1", 1); len(got) != 1 || got[0].RunID != "a2" {
		t.Fatalf("limit=1 错误: %+v", got)
	}

	// 未知租户返回空，不报错。
	if got, err := s.ByTenant(context.Background(), "nope", 10); err != nil || len(got) != 0 {
		t.Fatalf("未知租户应返回空: %v %+v", err, got)
	}
}

func TestStoreForRunNotFound(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.ForRun(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreReturnsCopy(t *testing.T) {
	s, fs, _ := newStore(t)
	if err := fs.append(mkDiag("r1", "s1", testBase)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.All(context.Background(), 0)
	got[0].RunID = "MUTATED"
	got[0].Error = "MUTATED"
	got[0].LastTool = "MUTATED"

	again, err := s.ForRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if again.RunID != "r1" || again.Error != "boom" || again.LastTool != "" {
		t.Fatalf("返回的是缓存本体，外部改动污染了内部: %+v", again)
	}
}

func TestStoreConcurrentWriteDoesNotCorruptFile(t *testing.T) {
	s, fs, path := newStore(t)
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- fs.append(mkDiag(fmt.Sprintf("c-%02d", i), "s1",
				testBase.Add(time.Duration(i)*time.Second)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.All(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("并发写后条数 = %d, want %d", len(got), n)
	}
	b, _ := os.ReadFile(path)
	var onDisk []Diagnosis
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("并发写把文件写坏了: %v", err)
	}
	if len(onDisk) != n {
		t.Fatalf("落盘条数 = %d, want %d", len(onDisk), n)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("并发写后权限 = %o, want 600", fi.Mode().Perm())
	}
}

// Open 与 Attach 共用同一个 dataDir 时必须拿到同一个实例，
// 否则 API / 回灌读的缓存永远看不到 Attach 写的诊断。
func TestStoreSharedByDataDir(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s1.(*fileStore) != s2.(*fileStore) {
		t.Fatal("同一个 dataDir 没有共享实例")
	}
}

// Pending 只返回「未消费 + 晚于 since + 同会话」的诊断；MarkConsumed 落盘且幂等。
// 这两条是回灌不重复注入的存储层契约。
func TestStoreMarkConsumedAndPending(t *testing.T) {
	s, fs, path := newStore(t)
	consumedAt := testBase.Add(10 * time.Hour)
	items := []Diagnosis{
		{RunID: "r1", SessionID: "s1", Status: "failed", Error: "e1", CreatedAt: testBase.Add(time.Hour)},
		{RunID: "r2", SessionID: "s1", Status: "failed", Error: "e2", CreatedAt: testBase.Add(2 * time.Hour), ConsumedAt: &consumedAt},
		{RunID: "r3", SessionID: "s1", Status: "failed", Error: "e3", CreatedAt: testBase.Add(30 * time.Minute)}, // 早于 since
		{RunID: "r4", SessionID: "s2", Status: "failed", Error: "e4", CreatedAt: testBase.Add(3 * time.Hour)},    // 别的会话
	}
	seedItems(t, fs, items)

	ctx := context.Background()
	since := testBase.Add(45 * time.Minute)
	pending, err := s.Pending(ctx, "s1", since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RunID != "r1" {
		t.Fatalf("Pending 应只返回 r1（未消费/晚于 since/同会话）, got %+v", pending)
	}

	// 标记 r1 已消费后，Pending 不再返回它；Recent 仍能看到（兼容既有读接口）。
	if err := s.MarkConsumed(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.Pending(ctx, "s1", since, 0)
	if len(pending) != 0 {
		t.Fatalf("消费后 Pending 应为空, got %+v", pending)
	}
	got, err := s.ForRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConsumedAt == nil {
		t.Fatal("MarkConsumed 后 ConsumedAt 未填")
	}
	recent, _ := s.Recent(ctx, "s1", 0)
	if len(recent) != 3 {
		t.Fatalf("Recent 应仍返回该会话全部 3 条, got %d", len(recent))
	}

	// 幂等：重复标记 / 标记不存在的 runID 都不报错。
	if err := s.MarkConsumed(ctx, "r1"); err != nil {
		t.Fatalf("重复 MarkConsumed 不该报错: %v", err)
	}
	if err := s.MarkConsumed(ctx, "nope"); err != nil {
		t.Fatalf("标记不存在的 runID 不该报错: %v", err)
	}

	// consumedAt 要真正落盘（omitempty：未消费的记录不带这个字段）。
	b, _ := os.ReadFile(path)
	var onDisk []map[string]any
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	seenConsumed := 0
	for _, d := range onDisk {
		if _, ok := d["consumedAt"]; ok {
			seenConsumed++
		}
	}
	if seenConsumed != 2 { // r1（刚标记）+ r2（预置）
		t.Fatalf("落盘的 consumedAt 条数 = %d, want 2", seenConsumed)
	}
}

func TestStorePendingDefaultLimitAndSinceZero(t *testing.T) {
	s, fs, _ := newStore(t)
	var items []Diagnosis
	for i := 0; i < 25; i++ {
		items = append(items, mkDiag(fmt.Sprintf("p-%02d", i), "s1",
			testBase.Add(time.Duration(i)*time.Minute)))
	}
	seedItems(t, fs, items)
	// since 为零值时全部未消费诊断都算「晚于 since」；limit<=0 用默认上限。
	got, err := s.Pending(context.Background(), "s1", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != defaultRecentLimit {
		t.Fatalf("limit<=0 默认应 %d 条, got %d", defaultRecentLimit, len(got))
	}
	if got[0].RunID != "p-24" {
		t.Fatalf("未按 CreatedAt 倒序: 首 = %q", got[0].RunID)
	}
}
