package auth

// 第三类 token：会话级受限「沙箱 key」。
//
// 平台里一共三类 JWT，共用同一把 HS256 密钥，靠 scope 字段互斥：
//
//	平台 JWT      scope 恒为空       → Authenticate
//	LLM 虚拟 key  scope = "llm"      → VerifyVirtualKey
//	沙箱 key      scope = "sandbox"  → VerifySandboxToken
//
// 三条校验路径都必须显式拒绝另外两类：否则「给沙箱内 CLI 用的受限凭据」
// 就能顺着共用密钥被当成平台全权身份（或反过来）。Authenticate 的现有语义
// 「scope 非空一律拒绝」已经天然挡住了沙箱 key，这里不改它；新增的隔离点只在
// VerifySandboxToken 这一侧。
//
// 单独成文件而不是塞进 service.go：这类 token 的校验入口是包级函数
// VerifySandboxToken(svc Authenticator, token)——主控在 api 层持有的是
// auth.Authenticator 接口，不是 *Service，入口签名必须能被那样调用。

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// SandboxKeyScope 是沙箱 key 的 scope 值。它与空 scope（平台 JWT）、
// VirtualKeyScope（"llm"）互斥，是三类 token 唯一的区分标记。
const SandboxKeyScope = "sandbox"

// DefaultSandboxKeyTTL 沙箱 key 默认有效期，与设计文档一致（2h）。
//
// 注意「随会话销毁自然失效」不是 TTL 的功劳：会话存在性校验在 api 层
// trySandboxAuth（那里才有 Store），删会话即吊销；TTL 只作兜底，限制 key
// 泄露后没人删会话时的最大可利用窗口。auth 包没有 Store，不在这里做该校验。
//
// 单独一个常量而不是复用 DefaultVirtualKeyTTL：两类 token 的 TTL 策略此刻恰好都是
// 2h，但一个绑模型网关、一个绑沙箱内 CLI，将来调整其一不应悄悄改动另一个。
const DefaultSandboxKeyTTL = 2 * time.Hour

// SandboxClaims 是校验通过的沙箱 key 内容。
//
// 与 VirtualClaims 形状一致（都是会话级短期凭据），但这是刻意分开的导出类型：
// 调用方是 api 层窄白名单，它需要 SessionID 做「自己会话」的归属校验，
// 和平台身份（Principal）、LLM 网关凭据是三件不同的事。
type SandboxClaims struct {
	UserID    string
	TenantID  string
	SessionID string
	Scope     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// sandboxClaims 是沙箱 key 的 JWT payload。
//
// 与 virtualClaims 形状相同（tid / sid / scope / sub / iat / exp），差异只在 scope 的
// 取值与校验入口。保留独立结构体是为了让「这是第三类 token」在代码里一目了然，
// 而不是靠读 scope 字符串去猜。
type sandboxClaims struct {
	TenantID  string `json:"tid"`
	SessionID string `json:"sid"`
	Scope     string `json:"scope"`
	jwt.RegisteredClaims
}

// IssueSandboxKey 签发会话级受限沙箱 key。
//
// ttl <= 0 时取默认 2h。这里和 IssueVirtualKey 一样不接受调用方自定义 scope：
// scope 是三类 token 的分界，允许传参迟早会被传成空串，沙箱 key 就变成了
// 平台 token。scope 恒为 SandboxKeyScope，由本函数写死。
func (s *Service) IssueSandboxKey(tenantID, userID, sessionID string, ttl time.Duration) (string, time.Time, error) {
	if tenantID == "" || userID == "" || sessionID == "" {
		return "", time.Time{}, errors.New("auth: 沙箱 key 需要租户、用户、会话三者齐全")
	}
	if ttl <= 0 {
		ttl = DefaultSandboxKeyTTL
	}
	now := s.now()
	exp := now.Add(ttl)
	c := &sandboxClaims{
		TenantID:  tenantID,
		SessionID: sessionID,
		Scope:     SandboxKeyScope,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok, err := signHS256(s.secret, c)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// VerifySandboxToken 校验 api 层收到的沙箱 key。
//
// 签名是包级函数而不是 *Service 方法：主控在 internal/api/sandbox_authz.go 里
// 持有的是 auth.Authenticator（h.cfg.Auth），调用形态固定为
//
//	claims, err := auth.VerifySandboxToken(h.cfg.Auth, token)
//
// 只有具体类型 *Service 才有密钥与时钟，所以先用类型断言取回 *Service；
// 传进来的是别的 Authenticator 实现（测试替身 / 未来换实现）一律
// ErrInvalidCredential，fail-closed，不猜、不降级。
//
// 隔离顺序：先验签名与有效期（parseHS256，与另外两类共用），再验 scope，
// 最后验字段完整性。平台 JWT（scope 空）与虚拟 key（scope="llm"）签名都合法，
// 正是靠 c.Scope != SandboxKeyScope 这一步被挡在门外。
func VerifySandboxToken(svc Authenticator, token string) (SandboxClaims, error) {
	s, ok := svc.(*Service)
	// s == nil 挡住「Authenticator 里装了个 (*Service)(nil)」这种 typed-nil：
	// 类型断言会成功，但接着读 s.secret 会 panic。fail-closed 直接判无效。
	if !ok || s == nil {
		return SandboxClaims{}, ErrInvalidCredential
	}
	c := &sandboxClaims{}
	if err := parseHS256(s.secret, token, c, s.now); err != nil {
		return SandboxClaims{}, err
	}
	if c.Scope != SandboxKeyScope {
		return SandboxClaims{}, ErrInvalidCredential
	}
	if c.Subject == "" || c.TenantID == "" || c.SessionID == "" {
		return SandboxClaims{}, ErrInvalidCredential
	}
	// parseHS256 的 WithExpirationRequired 保证 ExpiresAt 非空；IssuedAt 不是必需项，
	// 手签的 token 可能没有 iat，这里兜住 nil，别让一次畸形输入把进程打 panic。
	var issued, expires time.Time
	if c.IssuedAt != nil {
		issued = c.IssuedAt.Time
	}
	if c.ExpiresAt != nil {
		expires = c.ExpiresAt.Time
	}
	return SandboxClaims{
		UserID:    c.Subject,
		TenantID:  c.TenantID,
		SessionID: c.SessionID,
		Scope:     c.Scope,
		IssuedAt:  issued,
		ExpiresAt: expires,
	}, nil
}
