package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openProxyWith 起一个可注入 Approver / TokenProvider / AskTimeout 的测试代理。
// 数据目录走环境变量是因为 egress.Config 没有 DataDir 字段（egress.go 冻结）。
func openProxyWith(t *testing.T, mutate func(*Config)) Controller {
	t.Helper()
	t.Setenv("EGRESS_DATA_DIR", t.TempDir())
	cfg := Config{
		ListenAddr:    "127.0.0.1:0",
		DefaultPolicy: Policy{Default: Deny},
		LogSize:       128,
	}
	mutate(&cfg)
	ctl, err := Open(cfg)
	if err != nil {
		t.Fatalf("启动测试代理: %v", err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	return ctl
}

// funcApprover 让每个用例就地写审批行为，不必为每种结论都建一个类型。
type funcApprover func(context.Context, ApprovalRequest) (ApprovalResult, error)

func (f funcApprover) Request(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
	return f(ctx, req)
}

// askPolicy 让回环地址命中一条 Ask 规则（AllowPrivate 必须开，否则私网拦截
// 会先于规则命中）。
func askPolicy() Policy {
	return Policy{
		Default:      Deny,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Ask}},
	}
}

func TestAskAllowConnectApprovesAndLogs(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure-ok")
	}))
	defer srv.Close()

	reqCh := make(chan ApprovalRequest, 1)
	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			reqCh <- req
			return ApprovalResult{Decision: "allow", ApprovalID: req.ToolCallID}, nil
		})
		c.AskTimeout = 2 * time.Second
	})
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", &tls.Config{InsecureSkipVerify: true})

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("审批通过后 CONNECT 隧道应成功: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "secure-ok" {
		t.Fatalf("响应 = %d %q", resp.StatusCode, body)
	}

	select {
	case req := <-reqCh:
		if !strings.HasPrefix(req.ToolCallID, "egress/127.0.0.1/") {
			t.Errorf("toolCallID 格式不对: %q", req.ToolCallID)
		}
		if req.Kind != "egress" || req.RunID != "run-1" || req.Host != "127.0.0.1" || req.Port == "" {
			t.Errorf("审批请求字段不全: %+v", req)
		}
		if req.Title == "" || req.Timeout != 2*time.Second {
			t.Errorf("Title/Timeout 不对: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("Approver 未被调用")
	}

	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool {
		return e.Decision == Allow && e.ApprovalID != ""
	})
	if e.Reason != "ask-approved" {
		t.Errorf("Reason = %q, want ask-approved", e.Reason)
	}
}

// 覆盖 handleForward（明文 HTTP absolute-URI）的 Ask 分支，确认审批放行后
// 请求真的转发到了上游，且日志回填了 ApprovalID。
func TestAskAllowForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "plain-ok")
	}))
	defer srv.Close()

	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			return ApprovalResult{Decision: "allow"}, nil
		})
		c.AskTimeout = 2 * time.Second
	})
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", nil)

	resp, err := client.Get(srv.URL + "/hello")
	if err != nil {
		t.Fatalf("审批通过后明文 HTTP 应成功: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "plain-ok" {
		t.Fatalf("响应 = %d %q", resp.StatusCode, body)
	}

	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool {
		return e.Decision == Allow && e.ApprovalID != ""
	})
	if e.Reason != "ask-approved" {
		t.Errorf("Reason = %q, want ask-approved", e.Reason)
	}
}

func TestAskDenyReturns403(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("被拒绝的 CONNECT 不该到达目标")
	}))
	defer srv.Close()

	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			return ApprovalResult{Decision: "deny", ApprovalID: req.ToolCallID}, nil
		})
		c.AskTimeout = 2 * time.Second
	})
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", &tls.Config{InsecureSkipVerify: true})

	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("deny 时 CONNECT 应失败")
	}
	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool {
		return e.Decision == Deny && e.ApprovalID != ""
	})
	if e.Reason != "ask-denied" {
		t.Errorf("Reason = %q, want ask-denied", e.Reason)
	}
}

// 假 Approver 永远不主动回复，只等 ctx；AskTimeout 到点按 deny 收尾。
func TestAskTimeoutFallsBackToDeny(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("超时的 CONNECT 不该到达目标")
	}))
	defer srv.Close()

	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			<-ctx.Done()
			return ApprovalResult{Decision: "timeout"}, nil
		})
		c.AskTimeout = 150 * time.Millisecond
	})
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", &tls.Config{InsecureSkipVerify: true})

	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("超时应按 deny，CONNECT 失败")
	}
	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool { return e.Decision == Deny })
	if e.Reason != "ask-timeout" {
		t.Errorf("Reason = %q, want ask-timeout", e.Reason)
	}
}

// 认不出 run（这里是有租户但 RunID 为空）时直接降级，Approver 不该被调用。
func TestAskNoRunDegradesDeny(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("无 runID 的 Ask 不该到达目标")
	}))
	defer srv.Close()

	var calls atomic.Int32
	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			calls.Add(1)
			return ApprovalResult{Decision: "allow"}, nil
		})
		c.AskTimeout = 2 * time.Second
	})
	ctl.Register("sb-norun", "t1", "u1", "sess-1", "") // 空 RunID
	client := proxyClient(t, ctl, "sb-norun", "nonce", &tls.Config{InsecureSkipVerify: true})

	if _, err := client.Get(srv.URL); err == nil {
		t.Fatal("无 runID 应降级 deny")
	}
	if calls.Load() != 0 {
		t.Error("前置条件不满足时不应调用 Approver")
	}
	e := waitEntry(t, ctl, "t1", func(e LogEntry) bool { return e.Decision == Deny })
	if e.Reason != "ask-no-run" {
		t.Errorf("Reason = %q, want ask-no-run", e.Reason)
	}
}

// 代理 Close 时必须唤醒挂在 Ask 上的 handler，不能卡到超时。
func TestAskCloseUnblocksPending(t *testing.T) {
	entered := make(chan struct{})
	ctl := openProxyWith(t, func(c *Config) {
		c.DefaultPolicy = askPolicy()
		c.Approver = funcApprover(func(ctx context.Context, req ApprovalRequest) (ApprovalResult, error) {
			close(entered)
			<-ctx.Done()
			return ApprovalResult{Decision: "deny"}, nil
		})
		c.AskTimeout = time.Minute // 唤醒应来自 Close，而不是超时
	})
	ctl.Register("sb-1", "t1", "u1", "sess-1", "run-1")
	client := proxyClient(t, ctl, "sb-1", "nonce", nil)

	done := make(chan struct{})
	go func() {
		_, _ = client.Get("https://127.0.0.1:1/")
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Approver 没进入挂起")
	}
	if err := ctl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 后挂起的出网连接没被唤醒")
	}
}
