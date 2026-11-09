package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/lifecycle"
	"github.com/WangYulong666/astra-agent/internal/store"
)

type rotatingTestIssuer struct{ calls int }

func (i *rotatingTestIssuer) IssueForSession(_ context.Context, scope SessionScope) (map[string]string, time.Time, error) {
	i.calls++
	if scope.TenantID != "tenant" || scope.UserID != "user" {
		return nil, time.Time{}, fmt.Errorf("lost ownership")
	}
	return map[string]string{"ASTRA_TEST_GENERATION": fmt.Sprint(i.calls)}, time.Now().Add(time.Hour), nil
}
func TestIdleReclaimPreservesFilesAndRestartsSession(t *testing.T) {
	issuer := &rotatingTestIssuer{}
	h := newHarness(t, func(c *Config) { c.Creds = issuer })
	ctx := context.Background()
	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{TenantID: "tenant", UserID: "user"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.m.cfg.Provider.Destroy(ctx, sess.ID) })
	path := filepath.Join(sess.Cwd, "keep.txt")
	if err := os.WriteFile(path, []byte("keep after reclaim"), 0600); err != nil {
		t.Fatal(err)
	}
	rp := lifecycle.New(lifecycle.Policy{IdleTimeout: time.Minute}, lifecycle.Deps{Provider: h.m.cfg.Provider, Store: h.st, WithIdleSession: h.m.WithIdleSession})
	rp.Now = func() time.Time { return sess.LastActiveAt.Add(2 * time.Minute) }
	rep, err := rp.Sweep(ctx)
	if err != nil || rep.Paused != 1 || rep.Destroyed != 0 {
		t.Fatalf("reclaim: %+v %v", rep, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "keep after reclaim" {
		t.Fatalf("file lost: %v", err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if done := h.waitTerminal(t, run.ID); done.Status != store.RunCompleted {
		t.Fatalf("resume failed: %+v", done)
	}
	// 等待执行信号量释放，再检查新启动是否重新签发会话凭据。
	binding, _ := h.m.bindingFor(sess.ID)
	binding.sem <- struct{}{}
	defer func() { <-binding.sem }()
	if issuer.calls != 2 {
		t.Fatalf("expected fresh credentials after restart, calls=%d", issuer.calls)
	}
	b, err = os.ReadFile(path)
	if err != nil || string(b) != "keep after reclaim" {
		t.Fatalf("file lost after resume: %v", err)
	}
}
func TestIdleGuardExcludesExecutionAndQueuedRuns(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.m.cfg.Provider.Destroy(ctx, sess.ID) })
	binding, _ := h.m.bindingFor(sess.ID)
	binding.sem <- struct{}{}
	acted, err := h.m.WithIdleSession(ctx, sess.ID, func() error { t.Fatal("reclaimed executing session"); return nil })
	<-binding.sem
	if err != nil || acted {
		t.Fatalf("busy guard: %v %v", acted, err)
	}
	if err := h.st.CreateRun(ctx, store.Run{ID: "queued", SessionID: sess.ID, Status: store.RunQueued}); err != nil {
		t.Fatal(err)
	}
	acted, err = h.m.WithIdleSession(ctx, sess.ID, func() error { t.Fatal("reclaimed queued session"); return nil })
	if err != nil || acted {
		t.Fatalf("queued guard: %v %v", acted, err)
	}
}
