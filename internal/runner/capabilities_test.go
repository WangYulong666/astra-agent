package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/egress"
)

// fakeEgress 只记录归属登记，不真起代理：这里测的是 runner 的注入与生命周期，
// 代理本身的转发行为由 internal/egress 的 e2e 测试覆盖。
type fakeEgress struct {
	mu      sync.Mutex
	regs    []fakeReg
	unregs  []string
	addr    string
	enabled bool
}

type fakeReg struct {
	sandboxID, tenantID, userID, sessionID, runID string
}

func (f *fakeEgress) Enabled() bool { return f.enabled }
func (f *fakeEgress) Addr() string  { return f.addr }
func (f *fakeEgress) Policy(context.Context, string) (egress.Policy, error) {
	return egress.DefaultPolicy(), nil
}
func (f *fakeEgress) SetPolicy(context.Context, string, egress.Policy) error { return nil }
func (f *fakeEgress) Log(context.Context, string, int) ([]egress.LogEntry, error) {
	return nil, nil
}
func (f *fakeEgress) Register(sid, tid, uid, sessID, runID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs = append(f.regs, fakeReg{sid, tid, uid, sessID, runID})
}
func (f *fakeEgress) Unregister(sid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregs = append(f.unregs, sid)
}
func (f *fakeEgress) Close() error { return nil }

func (f *fakeEgress) snapshot() ([]fakeReg, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReg(nil), f.regs...), append([]string(nil), f.unregs...)
}

// fakeConnectors 模拟「proxy 模式还没接 token 替换」——这是当前的默认配置。
// 关键断言：Inject 报错不能把会话创建挡下来，否则配了连接器就再也建不了会话。
type fakeConnectors struct{ injectErr error }

func (f fakeConnectors) Enabled() bool { return true }
func (f fakeConnectors) List() []connector.Connector {
	return nil
}
func (f fakeConnectors) ListCredentials(context.Context, string, string) ([]connector.Credential, error) {
	return []connector.Credential{{Name: "github"}}, nil
}
func (f fakeConnectors) AuthorizeURL(context.Context, string, string, string, string, string) (string, error) {
	return "", nil
}
func (f fakeConnectors) ParseState(string) (string, string, string, error) {
	return "", "", "", errors.New("not used")
}
func (f fakeConnectors) Exchange(context.Context, string, string, string, string, string) (connector.Credential, error) {
	return connector.Credential{}, errors.New("not used")
}
func (f fakeConnectors) PutKey(context.Context, string, string, string, string) error { return nil }
func (f fakeConnectors) Delete(context.Context, string, string, string) error         { return nil }
func (f fakeConnectors) Inject(context.Context, string, string, []string) (map[string]string, error) {
	return nil, f.injectErr
}
func (f fakeConnectors) TokenFor(context.Context, string, string, string) (string, error) {
	return "", errors.New("not used")
}
func (f fakeConnectors) ConnectorForHost(string) (connector.Connector, bool) {
	return connector.Connector{}, false
}

func TestCreateSessionInjectsPerSessionEgress(t *testing.T) {
	eg := &fakeEgress{addr: "127.0.0.1:17890", enabled: true}
	h := newHarness(t, func(c *Config) {
		c.Egress = eg
		c.Connectors = fakeConnectors{injectErr: connector.ErrInjectProxyUnimplemented}
		c.EgressBypass = "127.0.0.1:7788"
	})
	ctx := context.Background()

	// proxy 模式 Inject 会失败，会话仍必须能建起来（增强项不是前置条件）
	s1, err := h.m.CreateSession(ctx, CreateSessionOptions{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatalf("第一个会话创建失败: %v", err)
	}
	s2, err := h.m.CreateSession(ctx, CreateSessionOptions{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatalf("第二个会话创建失败: %v", err)
	}

	regs, _ := eg.snapshot()
	if len(regs) < 2 {
		t.Fatalf("每个会话都应登记出网归属，实际 %d 条: %v", len(regs), regs)
	}
	if regs[0].sandboxID == regs[1].sandboxID {
		t.Fatal("两个会话用了同一个 sandboxID，审计会全部混在一起")
	}
	if regs[0].tenantID != "t1" || regs[0].userID != "u1" || regs[0].sessionID != s1.ID {
		t.Fatalf("归属信息不对: %+v (会话 %s)", regs[0], s1.ID)
	}

	// 共享的 AgentSpec.Env 绝不能被写脏：下一个会话会继承别人的凭据
	if _, leaked := h.m.cfg.Agent.Env["HTTP_PROXY"]; leaked {
		t.Fatal("共享 AgentSpec.Env 被写入了 HTTP_PROXY，后续会话会继承上一个的代理凭据")
	}
	if _, leaked := h.m.cfg.Agent.Env["NO_PROXY"]; leaked {
		t.Fatal("共享 AgentSpec.Env 被写入了 NO_PROXY")
	}

	// 起 run 后应带上 runID 重新登记
	if _, err := h.m.StartRun(ctx, s1.ID, "hello"); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		regs, _ = eg.snapshot()
		found := false
		for _, r := range regs {
			if r.sandboxID == s1.ID && r.runID != "" {
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	regs, _ = eg.snapshot()
	withRun := false
	for _, r := range regs {
		if r.sandboxID == s1.ID && r.runID != "" {
			withRun = true
		}
	}
	if !withRun {
		t.Fatalf("StartRun 后没有带 runID 重新登记: %+v", regs)
	}

	// 删除会话要注销，否则登记表会随会话数无限增长
	if err := h.m.DeleteSession(ctx, s2.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, unregs := eg.snapshot()
	found := false
	for _, id := range unregs {
		if id == s2.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("删除会话后没注销出网归属: %v", unregs)
	}
}

func TestEgressDisabledKeepsEnvUntouched(t *testing.T) {
	eg := &fakeEgress{enabled: false} // Addr 为空、Enabled=false
	h := newHarness(t, func(c *Config) { c.Egress = eg })
	ctx := context.Background()
	if _, err := h.m.CreateSession(ctx, CreateSessionOptions{}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	regs, _ := eg.snapshot()
	if len(regs) != 0 {
		t.Fatalf("能力关闭时不该登记归属，实际 %v", regs)
	}
	if _, leaked := h.m.cfg.Agent.Env["HTTP_PROXY"]; leaked {
		t.Fatal("能力关闭时不应注入代理变量")
	}
}
