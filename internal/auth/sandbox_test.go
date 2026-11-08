package auth

// 第三类 token（沙箱 key）的测试。
//
// 复用 service_test.go 的 fixture / fakeStore / testClock。重点验的是「判定顺序与
// 三类 token 的隔离」：共用一把 HS256 密钥，靠 scope 互斥，任一校验入口都必须
// 拒绝另外两类。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// signSandboxTokenForTest 用测试密钥签一条自定义 claims 的 token，专门用来构造
// 「签名合法但字段缺失 / scope 不对」这类边界输入——这些没法通过公开的
// IssueSandboxKey 造出来（它会先把字段补齐、scope 写死）。
func signSandboxTokenForTest(t *testing.T, f *fixture, mutate func(*sandboxClaims)) string {
	t.Helper()
	now := f.clock.Now()
	c := &sandboxClaims{
		TenantID:  f.tenant.ID,
		SessionID: "ses_1",
		Scope:     SandboxKeyScope,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   f.user.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}
	if mutate != nil {
		mutate(c)
	}
	tok, err := signHS256(testSecret, c)
	if err != nil {
		t.Fatalf("签测试 sandbox token 失败: %v", err)
	}
	return tok
}

// ── 签发 / 校验往返 ──

func TestSandboxKeyRoundTrip(t *testing.T) {
	f := newFixture(t)

	tok, exp, err := f.svc.IssueSandboxKey(f.tenant.ID, f.user.ID, "ses_1", 0) // 0 → 默认 TTL
	if err != nil {
		t.Fatalf("签发沙箱 key 失败: %v", err)
	}
	if want := f.clock.Now().Add(DefaultSandboxKeyTTL); !exp.Equal(want) {
		t.Fatalf("默认过期时间应为 %v，实际 %v", want, exp)
	}

	c, err := VerifySandboxToken(f.svc, tok)
	if err != nil {
		t.Fatalf("校验沙箱 key 失败: %v", err)
	}
	if c.UserID != f.user.ID || c.TenantID != f.tenant.ID || c.SessionID != "ses_1" {
		t.Fatalf("claims 身份不对: %+v", c)
	}
	if c.Scope != SandboxKeyScope {
		t.Fatalf("scope 应为 %q，实际 %q", SandboxKeyScope, c.Scope)
	}
	if c.IssuedAt.IsZero() || !c.IssuedAt.Equal(f.clock.Now()) {
		t.Fatalf("IssuedAt 不对: %v（应为 %v）", c.IssuedAt, f.clock.Now())
	}
	if !c.ExpiresAt.Equal(exp) {
		t.Fatalf("ExpiresAt 应为 %v，实际 %v", exp, c.ExpiresAt)
	}

	// 自定义 TTL 生效（不是永远走默认）。
	tok2, exp2, err := f.svc.IssueSandboxKey(f.tenant.ID, f.user.ID, "ses_2", 30*time.Minute)
	if err != nil {
		t.Fatalf("自定义 TTL 签发失败: %v", err)
	}
	if want := f.clock.Now().Add(30 * time.Minute); !exp2.Equal(want) {
		t.Fatalf("自定义过期时间应为 %v，实际 %v", want, exp2)
	}
	c2, err := VerifySandboxToken(f.svc, tok2)
	if err != nil || c2.SessionID != "ses_2" {
		t.Fatalf("自定义 TTL 的 token 校验失败: c2=%+v err=%v", c2, err)
	}
}

func TestSandboxKeyExpired(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.IssueSandboxKey(f.tenant.ID, f.user.ID, "ses_1", time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	f.clock.Advance(time.Hour + time.Minute)
	if _, err := VerifySandboxToken(f.svc, tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期沙箱 key 应返回 ErrExpired，实际 %v", err)
	}
}

// TestSandboxKeyTamperedSignature 篡改 payload 或签名后原样拼回，必须按无效凭据拒绝。
func TestSandboxKeyTamperedSignature(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.IssueSandboxKey(f.tenant.ID, f.user.ID, "ses_1", time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token 分段数异常: %d", len(parts))
	}

	// 改 payload（把 sid 换成别人的会话），签名留旧。
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload 解码失败: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	claims["sid"] = "ses_evil"
	patched, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("payload 编码失败: %v", err)
	}
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(patched) + "." + parts[2]
	if _, err := VerifySandboxToken(f.svc, forged); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("篡改 payload 应返回 ErrInvalidCredential，实际 %v", err)
	}

	// 签名段翻一个字符，同样拒绝。
	sig := parts[2]
	flip := byte('A')
	if sig[len(sig)-1] == 'A' {
		flip = 'B'
	}
	badSig := sig[:len(sig)-1] + string(flip)
	if _, err := VerifySandboxToken(f.svc, parts[0]+"."+parts[1]+"."+badSig); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("篡改签名应返回 ErrInvalidCredential，实际 %v", err)
	}

	// 换把密钥签的 token、以及纯垃圾串，都不能通过。
	other, err := signHS256([]byte("another-secret-0123456789-abcdefghij"),
		&sandboxClaims{
			TenantID: f.tenant.ID, SessionID: "ses_1", Scope: SandboxKeyScope,
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: f.user.ID, IssuedAt: jwt.NewNumericDate(f.clock.Now()),
				ExpiresAt: jwt.NewNumericDate(f.clock.Now().Add(time.Hour)),
			},
		})
	if err != nil {
		t.Fatalf("签对照 token 失败: %v", err)
	}
	if _, err := VerifySandboxToken(f.svc, other); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("别的密钥签的 token 应被拒，实际 %v", err)
	}
	for _, junk := range []string{"", "not-a-jwt", "a.b.c"} {
		if _, err := VerifySandboxToken(f.svc, junk); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("垃圾串 %q 应返回 ErrInvalidCredential，实际 %v", junk, err)
		}
	}
}

// ── 三类 token 交叉拒绝 ──

// TestSandboxKeyThreeWayIsolation 是这一层最要紧的安全用例：三类 token 共用密钥，
// 三条校验入口各自只认自己那一类。任何一条缝漏了，受限凭据就能升级成平台身份。
func TestSandboxKeyThreeWayIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tid, uid, sid := f.tenant.ID, f.user.ID, "ses_1"

	platform, _, err := f.svc.Issue(f.user) // scope ""
	if err != nil {
		t.Fatalf("签发平台 JWT 失败: %v", err)
	}
	virtual, _, err := f.svc.IssueVirtualKey(tid, uid, sid, time.Hour) // "llm"
	if err != nil {
		t.Fatalf("签发虚拟 key 失败: %v", err)
	}
	sandbox, _, err := f.svc.IssueSandboxKey(tid, uid, sid, time.Hour) // "sandbox"
	if err != nil {
		t.Fatalf("签发沙箱 key 失败: %v", err)
	}

	t.Run("各自入口接受自己", func(t *testing.T) {
		if _, err := f.svc.Authenticate(ctx, platform); err != nil {
			t.Fatalf("Authenticate 应接受平台 JWT，实际 %v", err)
		}
		if _, err := f.svc.VerifyVirtualKey(virtual); err != nil {
			t.Fatalf("VerifyVirtualKey 应接受虚拟 key，实际 %v", err)
		}
		if _, err := VerifySandboxToken(f.svc, sandbox); err != nil {
			t.Fatalf("VerifySandboxToken 应接受沙箱 key，实际 %v", err)
		}
	})

	t.Run("Authenticate 拒绝另外两类", func(t *testing.T) {
		if _, err := f.svc.Authenticate(ctx, virtual); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("虚拟 key 不能当平台 token，实际 %v", err)
		}
		if _, err := f.svc.Authenticate(ctx, sandbox); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("沙箱 key 不能当平台 token，实际 %v", err)
		}
	})

	t.Run("VerifyVirtualKey 拒绝另外两类", func(t *testing.T) {
		if _, err := f.svc.VerifyVirtualKey(platform); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("平台 JWT 不能当虚拟 key，实际 %v", err)
		}
		if _, err := f.svc.VerifyVirtualKey(sandbox); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("沙箱 key 不能当虚拟 key，实际 %v", err)
		}
	})

	t.Run("VerifySandboxToken 拒绝另外两类", func(t *testing.T) {
		if _, err := VerifySandboxToken(f.svc, platform); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("平台 JWT 不能当沙箱 key，实际 %v", err)
		}
		if _, err := VerifySandboxToken(f.svc, virtual); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("虚拟 key 不能当沙箱 key，实际 %v", err)
		}
	})

	// 附带：refresh / SSE 两条入口也不能把沙箱 key 升级成长期 / 事件流票据。
	t.Run("沙箱 key 不能冒充 refresh / SSE 票据", func(t *testing.T) {
		if _, err := f.svc.Refresh(ctx, sandbox); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("沙箱 key 不能当 refresh，实际 %v", err)
		}
		if _, err := f.svc.RedeemSSETicket(ctx, sandbox); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("沙箱 key 不能当 SSE 票据，实际 %v", err)
		}
	})
}

// ── 字段完整性 ──

func TestIssueSandboxKeyRequiresIDs(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ name, tid, uid, sid string }{
		{"缺租户", "", f.user.ID, "ses_1"},
		{"缺用户", f.tenant.ID, "", "ses_1"},
		{"缺会话", f.tenant.ID, f.user.ID, ""},
		{"全空", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := f.svc.IssueSandboxKey(tc.tid, tc.uid, tc.sid, time.Hour); err == nil {
				t.Fatal("缺字段时应拒绝签发")
			}
		})
	}
}

// TestVerifySandboxTokenRejectsIncompleteClaims 用「签名合法」的畸形 token 证明
// scope 校验与字段完整性校验都在 parseHS256 之后真正生效。
func TestVerifySandboxTokenRejectsIncompleteClaims(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name   string
		mutate func(*sandboxClaims)
	}{
		{"scope 为平台（空）", func(c *sandboxClaims) { c.Scope = "" }},
		{"scope 为虚拟 key（llm）", func(c *sandboxClaims) { c.Scope = VirtualKeyScope }},
		{"scope 为其它值", func(c *sandboxClaims) { c.Scope = SSETicketScope }},
		{"缺会话 sid", func(c *sandboxClaims) { c.SessionID = "" }},
		{"缺租户 tid", func(c *sandboxClaims) { c.TenantID = "" }},
		{"缺用户 sub", func(c *sandboxClaims) { c.Subject = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := signSandboxTokenForTest(t, f, tc.mutate)
			if _, err := VerifySandboxToken(f.svc, tok); !errors.Is(err, ErrInvalidCredential) {
				t.Fatalf("应返回 ErrInvalidCredential，实际 %v", err)
			}
		})
	}
}

// ── VerifySandboxToken 的 Authenticator 参数 ──

// foreignAuthenticator 是一个**不是** *Service 的 Authenticator 实现。
type foreignAuthenticator struct{}

func (foreignAuthenticator) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, nil
}
func (foreignAuthenticator) Login(context.Context, string, string, string) (store.User, error) {
	return store.User{}, nil
}
func (foreignAuthenticator) Issue(store.User) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func TestVerifySandboxTokenRequiresConcreteService(t *testing.T) {
	f := newFixture(t)
	tok, _, err := f.svc.IssueSandboxKey(f.tenant.ID, f.user.ID, "ses_1", time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 传进来的是别的 Authenticator 实现，即便 token 完全合法也必须 fail-closed 拒绝。
	if _, err := VerifySandboxToken(foreignAuthenticator{}, tok); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("非 *Service 实现应返回 ErrInvalidCredential，实际 %v", err)
	}
	// nil 接口：断言直接失败，不能 panic。
	if _, err := VerifySandboxToken(nil, tok); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("nil Authenticator 应返回 ErrInvalidCredential，实际 %v", err)
	}
	// typed-nil *Service：类型断言会成功，但读 s.secret 会 panic——必须挡住。
	var typedNil Authenticator = (*Service)(nil)
	if _, err := VerifySandboxToken(typedNil, tok); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("typed-nil *Service 应返回 ErrInvalidCredential，实际 %v", err)
	}
}

// ── 编译期：runner.SandboxKeyIssuer 接口满足性 ──

// runner 的 SandboxKeyIssuer 是只含一个方法的窄接口。这里在 auth 包内用一条
// 等价的本地接口做编译期断言，证明 *Service 的方法签名与它完全吻合；
// 不直接 import runner 是因为会成环（runner → obs → auth），
// 真正的 runner.SandboxKeyIssuer 断言放在外部测试包 sandbox_issuer_test.go。
type sandboxKeyIssuerShape interface {
	IssueSandboxKey(tenantID, userID, sessionID string, ttl time.Duration) (string, time.Time, error)
}

var _ sandboxKeyIssuerShape = (*Service)(nil)
