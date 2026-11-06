package app

// 能力之间的依赖倒置适配层。
//
// 装配顺序上有个环：egress 需要 runner 的审批 + connector 的 token，
// runner 又需要 egress 的代理。解法不是改接口，而是让 egress 持有一个
// 「先占位、后填实」的 lazy adapter：openEgress 时传空壳，
// runner / connector 建好后调用 Set 填进去。
//
// 空壳在没填实时的行为必须**保守**：审批按 deny、token 按「没有」，
// 不能因为装配还没完成就放行一次出网。

import (
	"context"
	"errors"
	"sync"

	"github.com/WangYulong666/astra-agent/internal/activity"
	"github.com/WangYulong666/astra-agent/internal/collab"
	"github.com/WangYulong666/astra-agent/internal/connector"
	"github.com/WangYulong666/astra-agent/internal/egress"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── egress.Approver ────────────────────────────────────────────

type lazyApprover struct {
	mu    sync.RWMutex
	inner egress.Approver
}

func (l *lazyApprover) Set(a egress.Approver) {
	l.mu.Lock()
	l.inner = a
	l.mu.Unlock()
}

func (l *lazyApprover) Request(ctx context.Context, req egress.ApprovalRequest) (egress.ApprovalResult, error) {
	l.mu.RLock()
	a := l.inner
	l.mu.RUnlock()
	if a == nil {
		return egress.ApprovalResult{Decision: "deny"}, nil
	}
	return a.Request(ctx, req)
}

type approverAdapter struct{ mgr *runner.Manager }

func (a approverAdapter) Request(ctx context.Context, req egress.ApprovalRequest) (egress.ApprovalResult, error) {
	return a.mgr.RequestEgressApproval(ctx, req)
}

// ── egress.TokenProvider ───────────────────────────────────────

type lazyTokenProvider struct {
	mu    sync.RWMutex
	inner egress.TokenProvider
}

func (l *lazyTokenProvider) Set(p egress.TokenProvider) {
	l.mu.Lock()
	l.inner = p
	l.mu.Unlock()
}

func (l *lazyTokenProvider) TokenFor(ctx context.Context, tid, uid, host string) (string, string, bool, error) {
	l.mu.RLock()
	p := l.inner
	l.mu.RUnlock()
	if p == nil {
		return "", "", false, nil
	}
	return p.TokenFor(ctx, tid, uid, host)
}

func (l *lazyTokenProvider) BaseFor(name string) (string, bool) {
	l.mu.RLock()
	p := l.inner
	l.mu.RUnlock()
	if p == nil {
		return "", false
	}
	return p.BaseFor(name)
}

func (l *lazyTokenProvider) NameFor(host string) (string, bool) {
	l.mu.RLock()
	p := l.inner
	l.mu.RUnlock()
	if p == nil {
		return "", false
	}
	return p.NameFor(host)
}

type tokenAdapter struct{ mgr connector.Manager }

func (a tokenAdapter) TokenFor(ctx context.Context, tid, uid, host string) (string, string, bool, error) {
	c, ok := a.mgr.ConnectorForHost(host)
	if !ok {
		return "", "", false, nil
	}
	tok, err := a.mgr.TokenFor(ctx, tid, uid, c.Name)
	if err != nil {
		// 「该用户没连过」是正常业务状态，不是系统故障：sidecar 要回 401
		// 让 agent 去授权，而不是回 502 让人去查日志。
		if errors.Is(err, connector.ErrNotConnected) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if tok == "" {
		return "", "", false, nil
	}
	header := c.AuthHeader
	if header == "" {
		header = "Authorization"
	}
	return header, tok, true, nil
}

func (a tokenAdapter) BaseFor(name string) (string, bool) {
	for _, c := range a.mgr.List() {
		if c.Name == name && c.APIBase != "" {
			return c.APIBase, true
		}
	}
	return "", false
}

func (a tokenAdapter) NameFor(host string) (string, bool) {
	c, ok := a.mgr.ConnectorForHost(host)
	if !ok {
		return "", false
	}
	return c.Name, true
}

// ── collab.Sessions ────────────────────────────────────────────

type sessionsAdapter struct{ mgr *runner.Manager }

func (a sessionsAdapter) CreateSession(ctx context.Context, o collab.CreateOpts) (collab.SessionInfo, error) {
	s, err := a.mgr.CreateSession(ctx, runner.CreateSessionOptions{
		ParentSessionID: o.ParentSessionID,
		TenantID:        o.TenantID,
		UserID:          o.UserID,
	})
	if err != nil {
		return collab.SessionInfo{}, err
	}
	return collab.SessionInfo{ID: s.ID}, nil
}

func (a sessionsAdapter) StartRun(ctx context.Context, sid, input, parentRunID string) (collab.RunInfo, error) {
	r, err := a.mgr.StartRunWithParent(ctx, sid, input, parentRunID)
	if err != nil {
		return collab.RunInfo{}, err
	}
	return collab.RunInfo{ID: r.ID}, nil
}

func (a sessionsAdapter) CancelRun(ctx context.Context, runID string) error {
	return a.mgr.Cancel(ctx, runID)
}

// ── activity 的两个来源 ────────────────────────────────────────

type egressLogAdapter struct{ ctl egress.Controller }

func (a egressLogAdapter) Log(ctx context.Context, tenantID string, limit int) ([]activity.EgressEntry, error) {
	raw, err := a.ctl.Log(ctx, tenantID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]activity.EgressEntry, 0, len(raw))
	for _, e := range raw {
		out = append(out, activity.EgressEntry{
			Time: e.Time, RunID: e.RunID, SessionID: e.SessionID,
			Host: e.Host, Decision: string(e.Decision), Reason: e.Reason,
		})
	}
	return out, nil
}

type childListerAdapter struct{ coord collab.Coordinator }

func (a childListerAdapter) List(ctx context.Context, parentRunID string) ([]activity.ChildEntry, error) {
	raw, err := a.coord.List(ctx, parentRunID)
	if err != nil {
		return nil, err
	}
	out := make([]activity.ChildEntry, 0, len(raw))
	for _, c := range raw {
		out = append(out, activity.ChildEntry{
			RunID: c.RunID, Title: c.Title, Status: c.Status,
			Summary: c.Summary, Error: c.Error, EndedAt: c.EndedAt,
		})
	}
	return out, nil
}

// 编译期断言：store.Store 必须满足 activity.RunReader（接口隐式实现，
// 写在这里是为了让「改了 Store 接口却忘了账本」在编译期就暴露）。
var _ activity.RunReader = (store.Store)(nil)
