package auth

// 认证服务的业务侧：把「一串 token」翻译成 Principal。
//
// 三条入口，热度完全不同：
//   - Authenticate 每个请求都走，必须快：JWT 只验签，API Key 查一次库且记账异步。
//   - Login 只在登录时走，要防账号枚举，也得扛住 bcrypt 的慢。
//   - Issue 只在登录成功后走。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/WangYulong666/astra-agent/internal/store"
)

const (
	// DefaultTokenTTL 登录 token 的默认有效期。24h 是「一天一班」的折中：
	// 再长则被偷走的窗口太大，再短则用户一天要登好几次。
	DefaultTokenTTL = 24 * time.Hour

	// DefaultRefreshTTL refresh token 的有效期。30 天内用户无需重新输密码；
	// 它只出现在 /v1/auth/refresh 一条路上，被偷的影响见 Refresh 的注释。
	DefaultRefreshTTL = 30 * 24 * time.Hour

	// DefaultSSETicketTTL SSE 票据有效期。60 秒只覆盖「拿票 → 立刻连上」
	// 这一瞬间的窗口；票据会进 URL（EventSource 的限制），长有效期等于把
	// 凭据留在访问日志和浏览器历史里。
	DefaultSSETicketTTL = 60 * time.Second

	// DefaultVirtualKeyTTL 虚拟 key 默认有效期，与设计文档一致（2h）。
	DefaultVirtualKeyTTL = 2 * time.Hour

	// VirtualKeyScope 虚拟 key 的 scope 值。它和空 scope 是两种 token 唯一的区分标记。
	VirtualKeyScope = "llm"

	// SSETicketScope SSE 票据的 scope 值。带它的 token 只能换一次事件流连接。
	SSETicketScope = "sse"

	// RefreshTokenType refresh token 的 typ 值。access token 恒为空。
	RefreshTokenType = "refresh"

	// minSecretLen HS256 密钥下限。短于 16 字节的密钥有被离线暴力猜解的现实风险。
	minSecretLen = 16

	// touchTimeout 异步更新 last_used_at 的兜底上限。
	touchTimeout = 2 * time.Second

	// jtiSweepInterval 已用 jti 的过期清理周期。票据 TTL 只有 60s，
	// 每分钟扫一遍足以让集合规模维持在「每分钟签发量」以下，不会无限涨。
	jtiSweepInterval = time.Minute
)

// Config 是构造 Service 的入参。零值字段走默认值，只有 Store 与 Secret 是必填。
type Config struct {
	Store  store.AdminStore
	Secret []byte
	TTL    time.Duration // 默认 DefaultTokenTTL
	Now    Clock         // 默认 time.Now
}

// Service 实现 Authenticator。
//
// 为什么是 struct 而不是一组闭包：密钥、TTL、时钟要一起传下去，
// 而且状态校验（用户/租户是否还活着）必须和签名校验绑在同一处，不能被绕过。
type Service struct {
	store  store.AdminStore
	secret []byte
	ttl    time.Duration
	now    Clock

	// usedJTI 记录已消费的 SSE 票据 jti → 过期时间，保证一次性。
	// 只存票据（TTL 60s），过期项由 sweepUsedJTI 周期清理，规模有上界；
	// 内存态意味着重启即清空——重启瞬间的在途票据本就活不过 60s，可接受。
	mu        sync.Mutex
	usedJTI   map[string]time.Time
	lastSweep time.Time
}

var _ Authenticator = (*Service)(nil)

// New 构造 Service。
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: Config.Store 不能为空")
	}
	if len(cfg.Secret) < minSecretLen {
		return nil, fmt.Errorf("auth: JWT 密钥至少需要 %d 字节", minSecretLen)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTokenTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// 复制一份密钥：调用方之后复用或改写那个切片，不应该影响已签出的 token。
	return &Service{
		store:     cfg.Store,
		secret:    append([]byte(nil), cfg.Secret...),
		ttl:       cfg.TTL,
		now:       cfg.Now,
		usedJTI:   map[string]time.Time{},
		lastSweep: cfg.Now(),
	}, nil
}

// Authenticate 按前缀分流。
//
// 判断依据只有前缀：JWT 是 base64url 的 `x.y.z`，不可能以 astra_sk_ 开头，
// 所以这个分流不会把合法 token 送错路径。
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Principal{}, ErrNoCredential
	}
	if strings.HasPrefix(token, APIKeyPrefix) {
		return s.authenticateAPIKey(ctx, token)
	}
	return s.authenticateJWT(ctx, token)
}

// authenticateAPIKey 处理机器凭据。
//
// 顺序值得注意：先查 key 本身，再查用户、再查租户。把 key 放在最前面是因为
// 它是唯一能立刻否掉请求的信息（一次索引查询），后面几步才有必要做。
func (s *Service) authenticateAPIKey(ctx context.Context, token string) (Principal, error) {
	k, err := s.store.GetAPIKeyByHash(ctx, HashAPIKey(token))
	if err != nil {
		// 不存在与「摘要算错」在这里没有区别，都只是无效凭据。
		return Principal{}, ErrInvalidCredential
	}
	if !k.RevokedAt.IsZero() {
		return Principal{}, ErrInvalidCredential
	}
	// 自己判过期而不是用 k.Revoked(): 后者内部用 time.Now，绕过 Clock 就没法测了。
	if !k.ExpiresAt.IsZero() && !s.now().Before(k.ExpiresAt) {
		return Principal{}, ErrExpired
	}

	u, err := s.store.GetUser(ctx, k.UserID)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	// 用户归属必须和 key 的租户一致：key 只要被改挂到别的租户，
	// 后面所有按 principal.TenantID 做的隔离都会跟着错。
	if u.TenantID != k.TenantID {
		return Principal{}, ErrInvalidCredential
	}
	if u.Status != store.StatusActive {
		return Principal{}, ErrInvalidCredential
	}

	t, err := s.store.GetTenant(ctx, k.TenantID)
	if err != nil {
		return Principal{}, ErrInvalidCredential
	}
	if t.Status != store.StatusActive {
		return Principal{}, ErrTenantSuspended
	}

	s.touchAsync(k.ID)
	return Principal{
		UserID:   u.ID,
		TenantID: k.TenantID,
		Email:    u.Email,
		Role:     u.Role,
		Via:      ViaAPIKey,
		KeyID:    k.ID,
	}, nil
}

// authenticateJWT 处理人用的 token。
func (s *Service) authenticateJWT(ctx context.Context, token string) (Principal, error) {
	c := &platformClaims{}
	if err := parseHS256(s.secret, token, c, s.now); err != nil {
		return Principal{}, err
	}
	// 签名对但 scope 非空 → 这是网关的虚拟 key 或 SSE 票据，不能当平台身份用。
	// scope=sse 单独点名：它的语义是「只读事件流」，一旦混进普通认证路径，
	// 等于把一次性短票升级成了全权身份。
	if c.Scope != "" {
		return Principal{}, ErrInvalidCredential
	}
	// refresh token 只能走 Refresh，拿它当 access 用会把 30 天 TTL 引入热路径。
	if c.Typ == RefreshTokenType {
		return Principal{}, ErrInvalidCredential
	}
	if c.Subject == "" || c.TenantID == "" {
		return Principal{}, ErrInvalidCredential
	}

	u, err := s.checkUserTenant(ctx, c.Subject, c.TenantID)
	if err != nil {
		return Principal{}, err
	}
	// 改密码之后的整体失效：早于 tokenValidAfter 签发的 token 一律拒绝。
	// 判据用「iat 不严格晚于」而不是「早于」：iat 只有秒级精度，等于的情况
	// 可能是改密码前半秒签出的，宁可错杀（新登录重试一次即可）也不能放过。
	if !u.TokenValidAfter.IsZero() {
		if c.IssuedAt == nil || !c.IssuedAt.Time.After(u.TokenValidAfter) {
			return Principal{}, ErrInvalidCredential
		}
	}

	// 角色以库为准而不是 claims.Role：降权之后，旧 token 不该还带着老角色。
	return Principal{
		UserID:   u.ID,
		TenantID: u.TenantID,
		Email:    u.Email,
		Role:     u.Role,
		Via:      ViaJWT,
	}, nil
}

// checkUserTenant 确认用户与租户都还活着、且归属一致。
//
// 签名只说明「这串 token 是我们签的」，不说明用户还在。删号、停用、租户欠费
// 都必须每次查库确认——否则一个 token 就能在租户被停用后继续用满 24h。
// authenticateJWT 与 Refresh 共用：两条入口的失效条件必须完全一致，否则
// 「access 被停用挡住、refresh 却还能续」这种缝迟早被踩出来。
func (s *Service) checkUserTenant(ctx context.Context, userID, tenantID string) (store.User, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return store.User{}, ErrInvalidCredential
	}
	if u.TenantID != tenantID {
		return store.User{}, ErrInvalidCredential
	}
	if u.Status != store.StatusActive {
		return store.User{}, ErrInvalidCredential
	}

	t, err := s.store.GetTenant(ctx, tenantID)
	if err != nil {
		return store.User{}, ErrInvalidCredential
	}
	if t.Status != store.StatusActive {
		return store.User{}, ErrTenantSuspended
	}
	return u, nil
}

// touchAsync 异步更新 last_used_at。
//
// 这个字段只用于「这把钥匙最后一次动是什么时候」的展示，却要在每个请求上写一次库：
// 同步做等于给热路径强加一次写 I/O，高并发下就是瓶颈。写丢了无所谓，所以异步。
func (s *Service) touchAsync(keyID string) {
	go func() {
		// 用 WithoutCancel：客户端断开不该让这次记账半途而废；
		// 再套短超时兜底，避免慢库把 goroutine 越堆越多。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), touchTimeout)
		defer cancel()
		_ = s.store.TouchAPIKey(ctx, keyID, s.now())
	}()
}

// Login 校验邮箱密码。
//
// 报错只有三种，且都不区分「用户不存在」和「密码错了」——区分了就等于免费送一个
// 账号枚举接口。租户停用单独报，因为 slug 本来就是公开的登录参数，不是秘密，
// 而且前端需要它来提示「你们公司被停用了」。
func (s *Service) Login(ctx context.Context, tenantSlug, email, password string) (store.User, error) {
	slug := strings.TrimSpace(tenantSlug)
	mail := strings.TrimSpace(email)
	if slug == "" || mail == "" || password == "" {
		return store.User{}, ErrInvalidCredential
	}

	t, err := s.store.GetTenantBySlug(ctx, slug)
	if err != nil {
		// 租户不存在也只是「凭据无效」：从响应上看不出这个 slug 有没有人用。
		return store.User{}, ErrInvalidCredential
	}
	if t.Status != store.StatusActive {
		return store.User{}, ErrTenantSuspended
	}

	u, err := s.store.GetUserByEmail(ctx, t.ID, mail)
	if err != nil {
		// 关键：用户不存在时也要花掉同样一次 bcrypt，否则「秒回」和「几百毫秒后回」
		// 就能被用来枚举邮箱。
		equalizeCheckCost(password)
		return store.User{}, ErrInvalidCredential
	}
	if err := CheckPassword(u.PasswordHash, password); err != nil {
		return store.User{}, ErrInvalidCredential
	}
	if u.Status != store.StatusActive {
		return store.User{}, ErrInvalidCredential
	}

	u.LastLoginAt = s.now()
	// 登录时间只是辅助信息，写失败不该让一次凭据正确的登录失败。
	_ = s.store.UpdateUser(ctx, u)
	return u, nil
}

// 用户不存在时也要花掉一次 bcrypt，见 equalizeCheckCost。
// 放包级而不是 Service 字段：这份假哈希与配置无关，整个进程算一次就够。
var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// equalizeCheckCost 用一份固定的假哈希跑一次校验，把「用户不存在」的耗时
// 拉到和「密码错」一致。否则响应快慢本身就是账号枚举信号，
// 错误信息统一成 ErrInvalidCredential 也没用。
func equalizeCheckCost(pw string) {
	dummyHashOnce.Do(func() {
		// 生成失败也不 panic：哈希为空时 CheckPassword 会立刻返回，
		// 顶多丢掉这层时间保护，不该让登录路径崩掉。
		dummyHash, _ = HashPassword("astra-dummy-password-not-a-credential")
	})
	_ = CheckPassword(dummyHash, pw)
}

// Issue 为已认证用户签发平台 JWT。
func (s *Service) Issue(u store.User) (string, time.Time, error) {
	if u.ID == "" || u.TenantID == "" {
		return "", time.Time{}, errors.New("auth: 用户缺少 ID 或租户，不能签发 token")
	}
	now := s.now()
	exp := now.Add(s.ttl)
	c := &platformClaims{
		TenantID: u.TenantID,
		Role:     u.Role,
		Email:    u.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
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

// IssuePair 同时签发 access + refresh（滚动续期用）。
//
// 单独一个方法而不是让调用方先 Issue 再 IssueRefresh：两种 token 必须来自
// 同一次登录决策，拆成两步迟早有人只发了一半。
func (s *Service) IssuePair(u store.User) (access string, accessExp time.Time, refresh string, refreshExp time.Time, err error) {
	access, accessExp, err = s.Issue(u)
	if err != nil {
		return "", time.Time{}, "", time.Time{}, err
	}
	refresh, refreshExp, err = s.issueRefresh(u)
	if err != nil {
		return "", time.Time{}, "", time.Time{}, err
	}
	return access, accessExp, refresh, refreshExp, nil
}

// issueRefresh 签发 refresh token。typ=refresh 是它与 access 唯一的区分，
// 两条校验路径（authenticateJWT / Refresh）都必须显式检查它。
func (s *Service) issueRefresh(u store.User) (string, time.Time, error) {
	if u.ID == "" || u.TenantID == "" {
		return "", time.Time{}, errors.New("auth: 用户缺少 ID 或租户，不能签发 refresh token")
	}
	now := s.now()
	exp := now.Add(DefaultRefreshTTL)
	c := &platformClaims{
		TenantID: u.TenantID,
		Email:    u.Email,
		Typ:      RefreshTokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
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

// Refresh 用 refresh token 换取一个新的用户身份（调用方据此签发新一对 token）。
//
// 安全语义：
//   - typ 必须是 refresh，access 冒充直接拒绝；
//   - 用户/租户状态与 tokenValidAfter 的检查和 access 完全一致——改密码、停用
//     之后旧 refresh 一样作废；
//   - 不维护「已轮换 refresh」黑名单：滚动续期只保证新鲜 token 不断签发，
//     旧 refresh 在其 30 天 TTL 内依然可用。要即时吊销，改密码（写
//     tokenValidAfter）或停用账号即可，二者都已覆盖。这是「不引入吊销表」
//     的自觉取舍，不是遗漏。
//
// 用户不存在、密码哈希不参与：refresh 只证明「这个账号曾经通过密码登录」。
func (s *Service) Refresh(ctx context.Context, token string) (store.User, error) {
	c := &platformClaims{}
	if err := parseHS256(s.secret, token, c, s.now); err != nil {
		return store.User{}, err
	}
	if c.Typ != RefreshTokenType {
		// access 或虚拟 key 冒充 refresh：签名合法但类型不对，一律按无效凭据。
		return store.User{}, ErrInvalidCredential
	}
	if c.Subject == "" || c.TenantID == "" {
		return store.User{}, ErrInvalidCredential
	}

	u, err := s.checkUserTenant(ctx, c.Subject, c.TenantID)
	if err != nil {
		return store.User{}, err
	}
	if !u.TokenValidAfter.IsZero() {
		if c.IssuedAt == nil || !c.IssuedAt.Time.After(u.TokenValidAfter) {
			return store.User{}, ErrInvalidCredential
		}
	}
	return u, nil
}

// IssueSSETicket 为已登录用户签发一张一次性事件流票据。
//
// 票据是 60 秒的 JWT：claim 带 scope="sse"、sub、tid 和随机 jti。jti 的
// 一次性由 RedeemSSETicket 的内存集合保证——同一张票第二次使用直接拒绝，
// 票据泄漏（它会出现在 URL 与访问日志里）的影响因此被压到「60 秒内、
// 只读事件流、先到先得」。
func (s *Service) IssueSSETicket(userID, tenantID string) (string, time.Time, error) {
	if userID == "" || tenantID == "" {
		return "", time.Time{}, errors.New("auth: SSE 票据需要用户与租户标识")
	}
	jti, err := RandomID()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: 生成 jti 失败: %w", err)
	}
	now := s.now()
	exp := now.Add(DefaultSSETicketTTL)
	c := &platformClaims{
		TenantID: tenantID,
		Scope:    SSETicketScope,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ID:        jti,
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

// RedeemSSETicket 校验并消费一张 SSE 票据，返回仅可用于只读授权的身份。
//
// 顺序很关键：先验签名与 scope，再原子地消费 jti，最后才查库确认用户/租户
// 状态。消费放在查库之前——即使用户恰好刚被停用，这张票也已经作废，
// 不会留下「失败请求还能重放」的口子。
func (s *Service) RedeemSSETicket(ctx context.Context, ticket string) (Principal, error) {
	c := &platformClaims{}
	if err := parseHS256(s.secret, ticket, c, s.now); err != nil {
		return Principal{}, err
	}
	if c.Scope != SSETicketScope {
		// scope 为空是 access，"llm" 是虚拟 key：都只能走各自的入口。
		return Principal{}, ErrInvalidCredential
	}
	if c.Subject == "" || c.TenantID == "" || c.ID == "" {
		return Principal{}, ErrInvalidCredential
	}
	if err := s.consumeJTI(c.ID, c.ExpiresAt.Time); err != nil {
		return Principal{}, err
	}

	u, err := s.checkUserTenant(ctx, c.Subject, c.TenantID)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		UserID:   u.ID,
		TenantID: u.TenantID,
		Email:    u.Email,
		Role:     u.Role,
		Via:      ViaJWT,
	}, nil
}

// consumeJTI 原子地标记 jti 已用。重复出现即重放，拒绝。
func (s *Service) consumeJTI(jti string, exp time.Time) error {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, used := s.usedJTI[jti]; used {
		return ErrInvalidCredential
	}
	s.usedJTI[jti] = exp
	s.sweepUsedJTI(now)
	return nil
}

// sweepUsedJTI 清掉已过期的 jti。调用方必须持有 s.mu。
//
// 周期触发而不是每次全扫：票据 TTL 60s，过期的 jti 留着也只是死数据，
// 每分钟清一遍即可让集合规模维持在「每分钟签发量」的量级。
func (s *Service) sweepUsedJTI(now time.Time) {
	if now.Sub(s.lastSweep) < jtiSweepInterval {
		return
	}
	s.lastSweep = now
	for k, exp := range s.usedJTI {
		if !now.Before(exp) {
			delete(s.usedJTI, k)
		}
	}
}

// VirtualClaims 是校验通过的虚拟 key 内容。
//
// 单独一个导出类型而不是复用 Principal：调用方是网关，它需要 SessionID、
// 也需要「这次调用属于哪次会话」的强约束，和平台身份是两回事。
type VirtualClaims struct {
	UserID    string
	TenantID  string
	SessionID string
	Scope     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// IssueVirtualKey 签发会话级短期 key。
//
// ttl <= 0 时取默认 2h。这里不接受调用方自定义 scope：
// scope 是两种 token 的分界，允许传参迟早会被传成空串，虚拟 key 就变成了平台 token。
func (s *Service) IssueVirtualKey(tenantID, userID, sessionID string, ttl time.Duration) (string, time.Time, error) {
	if tenantID == "" || userID == "" || sessionID == "" {
		return "", time.Time{}, errors.New("auth: 虚拟 key 需要租户、用户、会话三者齐全")
	}
	if ttl <= 0 {
		ttl = DefaultVirtualKeyTTL
	}
	now := s.now()
	exp := now.Add(ttl)
	c := &virtualClaims{
		TenantID:  tenantID,
		SessionID: sessionID,
		Scope:     VirtualKeyScope,
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

// VerifyVirtualKey 校验网关收到的虚拟 key。
//
// 与 authenticateJWT 共用一套签名校验，差异只在 scope 与字段完整性：
// 平台 JWT 的 scope 为空，到这里会被拒；反过来平台入口也会拒掉 scope 非空的 token。
// 这是两条链路唯一的、也是必须的隔离——它们用的是同一个密钥。
func (s *Service) VerifyVirtualKey(token string) (VirtualClaims, error) {
	c := &virtualClaims{}
	if err := parseHS256(s.secret, token, c, s.now); err != nil {
		return VirtualClaims{}, err
	}
	if c.Scope != VirtualKeyScope {
		return VirtualClaims{}, ErrInvalidCredential
	}
	if c.Subject == "" || c.TenantID == "" || c.SessionID == "" {
		return VirtualClaims{}, ErrInvalidCredential
	}
	return VirtualClaims{
		UserID:    c.Subject,
		TenantID:  c.TenantID,
		SessionID: c.SessionID,
		Scope:     c.Scope,
		IssuedAt:  c.IssuedAt.Time,
		ExpiresAt: c.ExpiresAt.Time,
	}, nil
}
