package runner

import (
	"context"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// 运行中删会话曾经返回 500：RemoveAll 和事件写入撞在一起，报 "directory not empty"，
// 而沙箱已经销毁——留下一个删不掉的半死会话。现在要求：删除必须成功，且数据干净。
func TestDeleteSessionDuringRun(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		if c.Agent.Env == nil {
			c.Agent.Env = map[string]string{}
		}
		// 拉长执行时间，保证删除时 run 真的还在跑
		c.Agent.Env["ASTRA_MOCK_DELAY_MS"] = "150"
	})
	ctx := context.Background()

	sess, err := h.m.CreateSession(ctx, CreateSessionOptions{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := h.m.StartRun(ctx, sess.ID, "一个长任务")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// 等它真的开始
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := h.st.GetRun(ctx, run.ID); err == nil && r.Status == store.RunRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := h.m.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	if _, err := h.st.GetSession(ctx, sess.ID); err == nil {
		t.Fatal("会话记录还在")
	}
	if runs, err := h.st.ListRuns(ctx, sess.ID); err == nil && len(runs) > 0 {
		t.Fatalf("run 记录没清干净: %d 条", len(runs))
	}
}
