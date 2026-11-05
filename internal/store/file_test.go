package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *File {
	t.Helper()
	s, err := OpenFile(t.TempDir(), Options{Sync: false})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRun(t *testing.T, s *File) (string, string) {
	t.Helper()
	ctx := context.Background()
	sid, rid := "sess1", "run1"
	if err := s.CreateSession(ctx, Session{ID: sid, SandboxID: sid}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.CreateRun(ctx, Run{ID: rid, SessionID: sid, Input: "hi"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return sid, rid
}

func TestAppendAssignsContiguousSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedRun(t, s)

	for i := 1; i <= 5; i++ {
		ev, err := s.Append(ctx, rid, NewEvent{Type: EventMessageDelta, Payload: map[string]any{"text": "x"}})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if ev.Seq != int64(i) {
			t.Errorf("第 %d 条 seq = %d，期望 %d", i, ev.Seq, i)
		}
	}

	got, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("回放 %d 条，期望 5", len(got))
	}
	for i, ev := range got {
		if ev.Seq != int64(i+1) {
			t.Errorf("第 %d 条 seq 乱序: %d", i, ev.Seq)
		}
		if string(ev.Payload) != `{"text":"x"}` {
			t.Errorf("payload 不符: %s", ev.Payload)
		}
	}
}

func TestEventsAfterSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedRun(t, s)
	for i := 0; i < 4; i++ {
		if _, err := s.Append(ctx, rid, NewEvent{Type: EventThoughtDelta, Payload: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Events(ctx, rid, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Seq != 3 {
		t.Fatalf("after_seq=2 应返回 seq 3,4，实际 %+v", got)
	}

	// 分页：limit 生效
	got, _ = s.Events(ctx, rid, 0, 2)
	if len(got) != 2 {
		t.Fatalf("limit=2 应返回 2 条，实际 %d", len(got))
	}
}

// 崩溃恢复的关键性质：重开之后 seq 必须接着走，不能重号。
// 重号会让客户端按 after_seq 续传时漏事件，而且 PG 里会直接主键冲突。
func TestSeqSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	s1, err := OpenFile(root, Options{Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	sid, rid := "sess1", "run1"
	if err := s1.CreateSession(ctx, Session{ID: sid}); err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateRun(ctx, Run{ID: rid, SessionID: sid}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s1.Append(ctx, rid, NewEvent{Type: EventAgentLog, Payload: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s1.Close()

	s2, err := OpenFile(root, Options{Sync: false})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	ev, err := s2.Append(ctx, rid, NewEvent{Type: EventAgentLog, Payload: map[string]any{"after": "reopen"}})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 4 {
		t.Fatalf("重开后 seq = %d，期望 4（不能重号）", ev.Seq)
	}

	all, _ := s2.Events(ctx, rid, 0, 0)
	if len(all) != 4 {
		t.Fatalf("重开后共 %d 条，期望 4", len(all))
	}
}

func TestConcurrentAppendNoDuplicateSeq(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, rid := seedRun(t, s)

	const workers, per = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := s.Append(ctx, rid, NewEvent{Type: EventAgentLog, Payload: map[string]any{"w": w}}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	all, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != workers*per {
		t.Fatalf("共 %d 条，期望 %d", len(all), workers*per)
	}
	seen := map[int64]bool{}
	for _, ev := range all {
		if seen[ev.Seq] {
			t.Fatalf("seq 重复: %d", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}

func TestTruncatedLineIsSkipped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sid, rid := seedRun(t, s)
	if _, err := s.Append(ctx, rid, NewEvent{Type: EventAgentLog, Payload: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	// 模拟进程被 kill 在写一半
	path := filepath.Join(s.sessionDir(sid), "runs", rid, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":2,"type":"agent_lo`)
	f.Close()

	all, err := s.Events(ctx, rid, 0, 0)
	if err != nil {
		t.Fatalf("坏行不该让回放失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("应只返回 1 条完整事件，实际 %d", len(all))
	}
}

func TestDeleteSessionRemovesRuns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sid, rid := seedRun(t, s)
	if _, err := s.Append(ctx, rid, NewEvent{Type: EventAgentLog, Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(ctx, sid); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetRun(ctx, rid); !errors.Is(err, ErrNotFound) {
		t.Errorf("删会话后 run 应不存在，实际 %v", err)
	}
	// 句柄也必须释放，否则同 id 重建会写到被删掉的 inode
	if err := s.CreateSession(ctx, Session{ID: sid}); err != nil {
		t.Fatalf("同 id 重建失败: %v", err)
	}
}

func TestRejectPathTraversal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateSession(ctx, Session{ID: "../../escape"}); err == nil {
		t.Error("非法会话 id 应被拒绝")
	}
	if _, err := s.Events(ctx, "../../etc/passwd", 0, 0); err == nil {
		t.Error("非法 run id 应被拒绝")
	}
}

func TestGetRunUnknown(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetRun(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("未知 run 应返回 ErrNotFound，实际 %v", err)
	}
}
