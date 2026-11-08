package api

import (
	"net/http"

	"github.com/WangYulong666/astra-agent/internal/auth"
)

// 沙箱凭据是第三类 token：与平台 JWT、LLM 虚拟 key 共用签名密钥但 scope 互斥。
// 这里做「识别 → 白名单 → 归属」三道检查，绝不让沙箱 key 变成平台 token。
//
// 骨架阶段返回 isSandbox=false（T4 完成后由主控接上 auth.Service.VerifySandboxKey）。
type sandboxCtxKey struct{}

func sandboxSessionID(r *http.Request) string {
	v, _ := r.Context().Value(sandboxCtxKey{}).(string)
	return v
}

// trySandboxAuth 返回 (principal, isSandbox, valid)。
func (h *handler) trySandboxAuth(r *http.Request) (auth.Principal, bool, bool) {
	return auth.Principal{}, false, false
}

// sandboxPathAllowed 是窄白名单：沙箱 key 只能访问能力发现、技能清单、meta，
// 以及**自己会话**下的 session/run/memory。其它一律拒绝。
func (h *handler) sandboxPathAllowed(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/v1/meta", p == "/v1/capabilities", p == "/v1/skills", p == "/v1/insights":
		return true
	case len(p) > len("/v1/capabilities/") && p[:len("/v1/capabilities/")] == "/v1/capabilities/":
		return true
	case len(p) > len("/v1/skills/") && p[:len("/v1/skills/")] == "/v1/skills/":
		return true
	case len(p) > len("/v1/memory/") && p[:len("/v1/memory/")] == "/v1/memory/":
		return true
	}
	return false
}

var _ = http.StatusOK
