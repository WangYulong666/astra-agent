package auth

// refresh token 与 SSE 票据的测试。
//
// 复用 service_test.go 的 fixture / fakeStore / testClock：这里要验的仍然是
// 「判定顺序与安全属性」——类型隔离、一次性消费、tokenValidAfter 整体失效。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// ── refresh token ──

// TestRefreshRotation 覆盖主流程：登录 → 签发对 → refresh 换新对 → 新 access 可用。
func TestRefreshRotation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	access, accessExp, refresh, refreshExp, err := f.svc.IssuePair(f.user)
	if err != nil {
		t.Fatalf("IssuePair 失败: %v", err)
	}
	if want := f.clock.Now().Add(DefaultRefreshTTL); !refreshExp.Equal(want) {
		t.Fatalf("refresh 过期时间应为 %v，实际 %v", want, refreshExp)
	}
	if accessExp.Before(refreshExp) == false {
		t.Fatalf("refresh 应比 access 长命: access=%v refresh=%v", accessExp, refreshExp)
	}

	// access 必须照常可用（refresh 的加入不能破坏原链路）。
	if _, err := f.svc.Authenticate(ctx, access); err != nil {
		t.Fatalf("access 校验失败: %v", err)
	}

	u, err := f.svc.Refresh(ctx, refresh)
	if err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	if u.ID != f.user.ID {
		t.Fatalf("Refresh 返回了错误的用户: %q", u.ID)
	}

	// 滚动续期：新对必须真的可用。
	access2, _, refresh2, _, err := f.svc.IssuePair(u)
	if err != nil {
		t.Fatalf("续期签发失败: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, access2); err != nil {
		t.Fatalf("续期后的 access 不可用: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, refresh2); err != nil {
		t.Fatalf("续期后的 refresh 不可用: %v", err)
	}
}

// TestRefreshRejectsAccess 冒充是这里最要紧的对抗用例：
// access 拿去 refresh、refresh 拿去当 access，两个方向都必须被拒。
// 否则 30 天的 refresh 等于把 access 的有效期放大 30 倍。
func TestRefreshRejectsAccess(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	access, _, refresh, _, err := f.svc.IssuePair(f.user)
	if err != nil {
		t.Fatalf("IssuePair 失败: %v", err)
	}

	if _, err := f.svc.Refresh(ctx, access); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("access 冒充 refresh 应被拒，实际 %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, refresh); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("refresh 冒充 access 应被拒，实际 %v", err)
	}

	// 虚拟 key（scope=llm）同样不能走 refresh 入口。
	vk, _, err := f.svc.IssueVirtualKey(f.tenant.ID, f.user.ID, "ses_1", time.Hour)
	if err != nil {
		t.Fatalf("签发虚拟 key 失败: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, vk); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("虚拟 key 冒充 refresh 应被拒，实际 %v", err)
	}
}

// TestTokenValidAfterInvalidatesTokens 改密码之后（tokenValidAfter 被更新），
// 旧 access 与旧 refresh 必须同时作废——这是「不维护黑名单」方案的验收线。
func TestTokenValidAfterInvalidatesTokens(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	access, _, refresh, _, err := f.svc.IssuePair(f.user)
	if err != nil {
		t.Fatalf("IssuePair 失败: %v", err)
	}

	// 模拟改密码：store 里的用户带上 tokenValidAfter（晚于刚才的签发时刻）。
	u := f.user
	u.TokenValidAfter = f.clock.Now().Add(time.Second)
	f.store.users[u.ID] = u

	if _, err := f.svc.Authenticate(ctx, access); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("改密码后旧 access 应被拒，实际 %v", err)
	}
	if _, err := f.svc.Refresh(ctx, refresh); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("改密码后旧 refresh 应被拒，实际 %v", err)
	}

	// 新签发的必须照常可用，否则用户自己也被锁在门外了。
	// 时钟前移越过 tokenValidAfter：真实流程里「改密码 → 重新登录」也是先后发生。
	f.clock.Advance(2 * time.Second)
	access2, _, refresh2, _, err := f.svc.IssuePair(u)
	if err != nil {
		t.Fatalf("改密码后签发失败: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, access2); err != nil {
		t.Fatalf("改密码后新 access 应可用: %v", err)
	}
	if _, err := f.svc.Refresh(ctx, refresh2); err != nil {
		t.Fatalf("改密码后新 refresh 应可用: %v", err)
	}

	// 停用账号与租户的检查由 checkUserTenant 承担，refresh 路径必须同样生效。
	suspended := u
	suspended.Status = store.StatusSuspended
	f.store.users[suspended.ID] = suspended
	if _, err := f.svc.Refresh(ctx, refresh2); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("停用后 refresh 应被拒，实际 %v", err)
	}
}

// ── SSE 票据 ──

// TestSSETicketLifecycle 覆盖签发 → 正常消费 → 重放被拒 → 过期被拒 → 冒用被拒。
func TestSSETicketLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	ticket, exp, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if want := f.clock.Now().Add(DefaultSSETicketTTL); !exp.Equal(want) {
		t.Fatalf("票据过期时间应为 %v，实际 %v", want, exp)
	}

	p, err := f.svc.RedeemSSETicket(ctx, ticket)
	if err != nil {
		t.Fatalf("消费失败: %v", err)
	}
	if p.UserID != f.user.ID || p.TenantID != f.tenant.ID {
		t.Fatalf("身份不对: %+v", p)
	}
	if p.Via != ViaJWT {
		t.Fatalf("Via 应为 %s，实际 %s", ViaJWT, p.Via)
	}

	// jti 一次性：同一张票第二次必须被拒，哪怕还在 60 秒有效期内。
	if _, err := f.svc.RedeemSSETicket(ctx, ticket); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("票据重放应被拒，实际 %v", err)
	}

	// 过期拒绝。
	ticket2, _, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("再次签发失败: %v", err)
	}
	f.clock.Advance(DefaultSSETicketTTL + time.Minute)
	if _, err := f.svc.RedeemSSETicket(ctx, ticket2); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期票据应返回 ErrExpired，实际 %v", err)
	}

	// 冒用一：票据放进 Authorization 当平台 token，authenticateJWT 必须拒绝。
	ticket3, _, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("再次签发失败: %v", err)
	}
	if _, err := f.svc.Authenticate(ctx, ticket3); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("SSE 票据冒充平台 token 应被拒，实际 %v", err)
	}
	// 冒用二：平台 access / 虚拟 key 走票据入口，scope 不对，同样拒绝。
	jwtTok, _, err := f.svc.Issue(f.user)
	if err != nil {
		t.Fatalf("签发 JWT 失败: %v", err)
	}
	if _, err := f.svc.RedeemSSETicket(ctx, jwtTok); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("access 冒充 SSE 票据应被拒，实际 %v", err)
	}
}

// TestSSETicketUserSuspended 票据签出后用户被停用，消费时必须查库拦住。
func TestSSETicketUserSuspended(t *testing.T) {
	f := newFixture(t)
	ticket, _, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	u := f.user
	u.Status = store.StatusSuspended
	f.store.users[u.ID] = u

	if _, err := f.svc.RedeemSSETicket(context.Background(), ticket); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("停用用户的票据应被拒，实际 %v", err)
	}
}

// TestSSETicketJTISwept 过期的 jti 必须被清理，集合不能无限涨。
func TestSSETicketJTISwept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// jti 在「消费」时入集合（签发不入，见 consumeJTI）。
	ticket1, _, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := f.svc.RedeemSSETicket(ctx, ticket1); err != nil {
		t.Fatalf("消费失败: %v", err)
	}
	if n := len(f.svc.usedJTI); n != 1 {
		t.Fatalf("消费后应记录 1 个 jti，实际 %d", n)
	}

	// 越过清理周期（1 分钟）与票据 TTL（60 秒）后再消费，触发 sweep。
	f.clock.Advance(2 * time.Minute)
	ticket2, _, err := f.svc.IssueSSETicket(f.user.ID, f.tenant.ID)
	if err != nil {
		t.Fatalf("再次签发失败: %v", err)
	}
	if _, err := f.svc.RedeemSSETicket(ctx, ticket2); err != nil {
		t.Fatalf("再次消费失败: %v", err)
	}
	if n := len(f.svc.usedJTI); n != 1 {
		t.Fatalf("sweep 后应只剩 1 个 jti，实际 %d", n)
	}
}

// TestIssueSSETicketRequiresIDs 缺标识时明确报错，不签发空票。
func TestIssueSSETicketRequiresIDs(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.IssueSSETicket("", f.tenant.ID); err == nil {
		t.Fatal("缺用户时应报错")
	}
	if _, _, err := f.svc.IssueSSETicket(f.user.ID, ""); err == nil {
		t.Fatal("缺租户时应报错")
	}
}
