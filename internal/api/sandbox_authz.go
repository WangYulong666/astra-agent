package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// 沙箱凭据是第三类 token：与平台 JWT（scope 空）、LLM 虚拟 key（scope=llm）
// 共用签名密钥但 scope 互斥。这里做「识别 → 会话存在性 → 白名单」三道检查，
// 绝不让沙箱 key 变成平台 token。
//
// 会话存在性校验就是吊销机制：删会话（软删标 SessionDeleted 或直接删目录）
// 后，已签发的沙箱 key 立刻失效，不必引入全局吊销表/黑名单，也不必轮换
// JWT 密钥。TTL 只是兜底，限制 key 泄露后的最大可利用窗口。
//
// 为什么不放开 /v1/sessions/{id} 与 /v1/runs/{id}：那需要对每条路径参数做
// session 归属校验，一旦漏一条就是横向越权。本轮先只放开「能力发现 / 技能清单 /
// meta / memory」——memory handler 本来就按登录身份的 tenant+user 过滤，
// 沙箱 key 填的 Principal 与之同源，无需额外路径校验。
type sandboxCtxKey struct{}

func sandboxSessionID(r *http.Request) string {
	v, _ := r.Context().Value(sandboxCtxKey{}).(string)
	return v
}

// trySandboxAuth 返回 (principal, isSandbox, valid)。
// isSandbox=false 表示「这不是沙箱 key」，主鉴权链会按平台 token 继续处理；
// 鉴权失败不能混进这个分支，否则会把一个坏平台 token 误当成沙箱凭据。
func (h *handler) trySandboxAuth(r *http.Request) (auth.Principal, bool, bool) {
	raw := bearerToken(r)
	if raw == "" || h.cfg.Auth == nil {
		return auth.Principal{}, false, false
	}
	claims, err := auth.VerifySandboxToken(h.cfg.Auth, raw)
	if err != nil {
		// 不是沙箱 key（或已过期/被篡改/scope 不符）。统一交回主鉴权链：
		// 那里会对非空但非法的 token 返回 401，不会因为这里「不认识」
		// 就降级成匿名访问。别用错误文本猜类型，那是迟早会漂的耦合。
		return auth.Principal{}, false, false
	}

	// 会话存在性校验 = 吊销。删会话后 key 在剩余 TTL 内仍能访问白名单端点是
	// P0 缺口：VerifySandboxToken 只验签名/exp/scope/字段，不看会话是否还活着。
	// Store 为 nil 时 fail-closed —— 无法证明会话仍存在就判凭据失效。这里刻意
	// 不留「Store 为 nil 就跳过校验」的静默放行：生产装配里 Store 必然非 nil，
	// 跳过校验只会把这次吊销修复退化成「配好了才生效」。测试夹具要显式传一个
	// 真实 store（见 sandbox_authz_test.go 的 OpenFile），不依赖 nil 捷径。
	if h.cfg.Store == nil {
		return auth.Principal{}, true, false
	}
	sess, err := h.cfg.Store.GetSession(r.Context(), claims.SessionID)
	if err != nil || sess.Status == store.SessionDeleted {
		// 查不到（目录已删 / 从未存在）或软删标记 = 会话已销毁，凭据作废。
		return auth.Principal{}, true, false
	}

	if !h.sandboxPathAllowed(r) {
		return auth.Principal{}, true, false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), sandboxCtxKey{}, claims.SessionID))
	return auth.Principal{
		UserID: claims.UserID, TenantID: claims.TenantID,
		Role: "member", Via: "sandbox",
	}, true, true
}

// sandboxPathAllowed 是窄白名单。authed 里沙箱分支直接放行、不再过 RBAC，
// 所以这个函数就是它的全部权限边界：新增端点必须显式加进来。
// trySandboxAuth 与 authed 都会调它（后者再兜一层 403），两处判断保持一致。
func (h *handler) sandboxPathAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	switch p {
	case "/v1/meta", "/v1/capabilities", "/v1/skills":
		return true
	}
	// 单项查询：最后一段必须是单个名字（不含 /），否则会连
	// /v1/skills/drafts、/v1/skills/drafts/{id}/accept 这类管理端点一起放行。
	for _, prefix := range []string{"/v1/capabilities/", "/v1/skills/"} {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		// 最后一段必须是单个名字：带 / 的（如 /v1/skills/drafts/{id}/accept）
		// 一律落到函数末尾的 return false，不再重复判一次 /v1/skills/ 前缀。
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		// skills 的 drafts 子命名空间是管理接口，明确排除。
		if prefix == "/v1/skills/" && strings.HasPrefix(rest, "drafts") {
			return false
		}
		return true
	}
	// memory 各端点本来就按登录身份的 tenant+user 过滤，无需路径归属校验。
	if strings.HasPrefix(p, "/v1/memory/") && len(p) > len("/v1/memory/") {
		return true
	}
	return false
}
