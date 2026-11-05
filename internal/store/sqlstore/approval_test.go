package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

func seedApproval(t *testing.T, s *Store, rid, tcid string) store.Approval {
	t.Helper()
	ctx := context.Background()
	a := store.Approval{
		RunID: rid, ToolCallID: tcid, Title: "写入 /tmp/x", Kind: "edit",
		Status:    store.ApprovalPending,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	if err := s.CreateApproval(ctx, a); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	return a
}

func TestApprovalLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedSession(t, s)
	seedApproval(t, s, rid, "tc1")

	got, err := s.GetApproval(ctx, rid, "tc1")
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.Status != store.ApprovalPending || got.Title != "写入 /tmp/x" || got.ExpiresAt.IsZero() {
		t.Fatalf("字段未往返: %+v", got)
	}

	list, err := s.ListPendingApprovals(ctx, rid)
	if err != nil || len(list) != 1 || list[0].ToolCallID != "tc1" {
		t.Fatalf("ListPendingApprovals: %+v err=%v", list, err)
	}

	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.ResolveApproval(ctx, rid, "tc1", store.ApprovalAllowed, "user", at); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	got, _ = s.GetApproval(ctx, rid, "tc1")
	if got.Status != store.ApprovalAllowed || got.ResolvedBy != "user" || !got.ResolvedAt.Equal(at) {
		t.Fatalf("审批未落终态: %+v", got)
	}
	if list, _ := s.ListPendingApprovals(ctx, rid); len(list) != 0 {
		t.Fatalf("已处理的审批仍在 pending 列表: %+v", list)
	}

	// 第二次点击必须是 ErrConflict，而不是静默覆盖成 deny
	if err := s.ResolveApproval(ctx, rid, "tc1", store.ApprovalDenied, "user", at); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("重复处理应 ErrConflict，实际 %v", err)
	}
	if err := s.ResolveApproval(ctx, rid, "nope", store.ApprovalDenied, "user", at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的审批应 ErrNotFound，实际 %v", err)
	}
}

// 并发点击：只能有一个 UPDATE 命中 pending，其余都是 ErrConflict。
func TestApprovalResolveIsAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedSession(t, s)
	seedApproval(t, s, rid, "tc-race")

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- s.ResolveApproval(ctx, rid, "tc-race", store.ApprovalAllowed, "user", time.Now().UTC())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var ok, conflict int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrConflict):
			conflict++
		default:
			t.Fatalf("意外错误: %v", err)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("成功 %d、冲突 %d，期望 1 / %d", ok, conflict, n-1)
	}
}

func TestExpirePendingApprovals(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedSession(t, s)
	seedApproval(t, s, rid, "tc1")
	seedApproval(t, s, rid, "tc2")

	at := time.Now().UTC()
	expired, err := s.ExpirePendingApprovals(ctx, at)
	if err != nil {
		t.Fatalf("ExpirePendingApprovals: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("过期 %d 条，应为 2", len(expired))
	}
	for _, a := range expired {
		if a.Status != store.ApprovalExpired || a.ResolvedBy != "system" {
			t.Fatalf("返回值未带终态: %+v", a)
		}
	}
	if list, _ := s.ListPendingApprovals(ctx, ""); len(list) != 0 {
		t.Fatalf("仍有 pending: %+v", list)
	}
	// 幂等：没有 pending 时再跑一次不该报错
	if again, err := s.ExpirePendingApprovals(ctx, at); err != nil || len(again) != 0 {
		t.Fatalf("二次过期: n=%d err=%v", len(again), err)
	}
}

// 老库升级：0001 已经记过版本，改 schema 文件不会重跑；只有 0002 能补上表。
// 这里把库伪装成「只应用过 0001」——删掉表与 0002 记录，再 Open 一次。
func TestApprovalTableUpgradesOldDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	s1, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, rid := seedSession(t, s1)
	if _, err := s1.db.ExecContext(ctx, "DROP TABLE approvals"); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	if _, err := s1.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = ?", "0002_approvals"); err != nil {
		t.Fatalf("DELETE migration: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open("sqlite://" + path)
	if err != nil {
		t.Fatalf("重开老库失败（0002 没有幂等补表）: %v", err)
	}
	defer s2.Close()
	if err := s2.CreateApproval(ctx, store.Approval{
		RunID: rid, ToolCallID: "tc-old", Status: store.ApprovalPending,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatalf("升级后仍无 approvals 表: %v", err)
	}
}
