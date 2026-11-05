package auth

// JWT 的签发与校验。
//
// 为什么单独一个文件：这一层只回答「这串 token 是不是我们用 HS256 签的、有没有过期」，
// 不碰数据库、不碰租户状态——那些在 service.go。分开之后，算法校验只有一处，
// 将来改动不会漏掉某条路径。

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// signingMethod 是唯一允许的算法。
//
// 写成包级变量而不是散落在各处写 jwt.SigningMethodHS256，是为了让「必须等于它」
// 这个判断只存在于一个地方：alg 混淆（none / RS256）是 JWT 最经典的漏洞，
// 允许的算法一旦分散在多处定义，迟早会有一处忘改。
var signingMethod = jwt.SigningMethodHS256

// platformClaims 是平台 JWT（登录后拿到的那串）的 payload。
//
// 用短名 sub/tid 而不是 userId/tenantId：token 每次请求都要过网络和日志，
// 长字段名纯属浪费。
type platformClaims struct {
	TenantID string `json:"tid"`
	Role     string `json:"role,omitempty"`
	Email    string `json:"email,omitempty"`
	// Typ 区分 access 与 refresh：access 恒为空，refresh 固定为 "refresh"。
	// 两种 token 共用一个密钥，不显式看一眼 typ，30 天的 refresh 就能被
	// 直接当 access 用——等于把 access 的有效期放大 30 倍。
	Typ string `json:"typ,omitempty"`
	// Scope 在平台 token 里恒为空。
	//
	// 保留这个字段不是为了用，而是为了能判断「这串东西是不是虚拟 key」：
	// 两种 token 共用一个密钥，不显式看一眼 scope，网关的 token 就能直接进 API。
	// SSE 票据（scope="sse"）同样靠它与平台身份隔离。
	Scope string `json:"scope,omitempty"`
	jwt.RegisteredClaims
}

// virtualClaims 是 LLM 网关用的短期 token，绑定到一次会话。
//
// 与 platformClaims 的差别就是多了 sid、scope 固定为 "llm"、不带 role/email：
// 它只授权「用这个会话调模型」，不授权任何平台管理动作。
type virtualClaims struct {
	TenantID  string `json:"tid"`
	SessionID string `json:"sid"`
	Scope     string `json:"scope"`
	jwt.RegisteredClaims
}

// signHS256 用 HS256 签发。
//
// 签发方刻意不接受调用方传入算法：能签的只有 HS256，就没有「配置错了签出 RS256」的可能。
func signHS256(secret []byte, claims jwt.Claims) (string, error) {
	return jwt.NewWithClaims(signingMethod, claims).SignedString(secret)
}

// parseHS256 校验签名与有效期，结果写进 dst。
//
// 失败原因收敛成两个：过期 → ErrExpired，其余（格式错、签名不对、算法不对、缺 exp）
// 一律 ErrInvalidCredential。调用方只关心「还能不能用」，细分原因只会泄漏信息给攻击者。
func parseHS256(secret []byte, token string, dst jwt.Claims, now Clock) error {
	_, err := jwt.ParseWithClaims(token, dst,
		func(t *jwt.Token) (any, error) {
			// 第二道闸：即使将来有人删掉下面的 WithValidMethods，这里也不会把 HMAC 密钥
			// 交出去——交给 RS256 的验签实现，等于把密钥当成公钥，攻击者可以自签任意 token。
			if t.Method.Alg() != signingMethod.Alg() {
				return nil, fmt.Errorf("auth: 不接受的签名算法 %q", t.Method.Alg())
			}
			return secret, nil
		},
		// 第一道闸：库在调用 keyfunc 之前就按 header 里的 alg 拒绝非白名单算法，
		// alg=none 死在这一步，连密钥都碰不到。
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		// 没有 exp 的 token 等于永久凭据，必须拒绝。库默认只「有则校验」。
		jwt.WithExpirationRequired(),
		// 时间源统一走 Clock，测试才能在不 sleep 的前提下把表拨到过期之后。
		jwt.WithTimeFunc(now),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return ErrExpired
		}
		return ErrInvalidCredential
	}
	return nil
}
