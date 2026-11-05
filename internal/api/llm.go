package api

// LLM 网关：虚拟 key 签发 + OpenAI 兼容透传 + 记账。
//
// 归属：T7 实现。
// 只在这里落地两个 handler，真正的代理逻辑与 provider 抽象在 internal/llm/。
//
// 这一层负责三件「只有它知道」的事，所以不能下放到 llm 包：
//  1. 令牌归属——虚拟 key 里的 tenant/user/session 来自 auth.Service，
//     而落库要的审计上下文（身份、来源 IP）来自 *http.Request。
//  2. 额度拦截——租户额度在 store.AdminStore 里，且必须发生在转发之前。
//  3. 状态码——请求不合法回 400、额度用尽回 429、上游挂了回 502。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/llm"
	"github.com/WangYulong666/astra-agent/internal/obs"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// gateway 进程级网关实例。
//
// 为什么是包级变量而不是 Config 字段：Config 定义在 api.go，属于并行开发的
// 公共文件，本任务不改它。网关天然「一进程一个」（它持有真 key，多份没有意义），
// 所以进程级注入不会出现多实例互相覆盖。
var gateway atomic.Pointer[llm.Gateway]

// SetLLMGateway 注入网关。必须在 api.Handler 之前调用；传 nil 表示关闭网关，
// 此时两个接口都回 501（本地不配 key 也能把控制面跑起来）。
func SetLLMGateway(g *llm.Gateway) { gateway.Store(g) }

func llmGateway() *llm.Gateway { return gateway.Load() }

// maxLLMBodyBytes 单次请求体上限。正常 chat 请求远小于这个数，
// 设上限是防止有人把网关当文件上传通道。
const maxLLMBodyBytes = 16 << 20

// issueLLMToken 签发会话级虚拟 key。
//
// 这是「真 key 不进沙箱」的入口：agent 拿到的是一张 2 小时的票据，
// 票据只证明「这是哪个租户/用户/会话」，不含任何上游凭据。
func (h *handler) issueLLMToken(w http.ResponseWriter, r *http.Request) {
	gw := llmGateway()
	if gw == nil {
		fail(w, http.StatusNotImplemented, "LLM 网关未启用（需要以 -llm-gateway 与 -llm-key 启动）")
		return
	}
	if h.cfg.Manager == nil {
		// 没有 manager 就拿不到会话，也就无从校验归属。
		fail(w, http.StatusNotImplemented, "会话管理未启用，无法签发 LLM 令牌")
		return
	}
	sess, ok := h.getOwnedSession(w, r)
	if !ok {
		return
	}

	// Authenticator 接口刻意不含虚拟 key 能力（平台 token 与网关 token 是两条链路，
	// 混进接口会让任何一个实现都被迫实现两条链路），只有具体类型才有。
	// 断言失败不 panic：回 501 并说清原因，比在请求路径上崩掉强。
	svc, ok := h.cfg.Auth.(*auth.Service)
	if !ok {
		fail(w, http.StatusNotImplemented, "当前认证实现不支持签发虚拟 key（需要启用 auth.Service）")
		return
	}

	// 会话上的归属优先（它就是这次调用的真实归属），历史会话可能没有这两个字段，
	// 才回落到当前身份。绝不从请求体读 tenant，否则等于让客户端指定计费对象。
	p := h.principal(r)
	tenantID := firstNonEmpty(sess.TenantID, p.TenantID)
	userID := firstNonEmpty(sess.UserID, p.UserID)
	if tenantID == "" || userID == "" {
		// 兼容模式（无认证）下没有租户身份：这种 token 签出去也无处记账，
		// 明确拒绝比签一张记不上账的票好。
		fail(w, http.StatusConflict, "会话没有租户归属，无法签发 LLM 令牌（请先启用认证与租户）")
		return
	}

	token, exp, err := svc.IssueVirtualKey(tenantID, userID, sess.ID, auth.DefaultVirtualKeyTTL)
	if err != nil {
		h.failErr(w, err)
		return
	}
	h.audit(r, "llm.token.issue", "session", sess.ID, map[string]any{
		"expiresAt": exp,
		"baseUrl":   gw.BasePath(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"expiresAt": exp,
		// baseUrl 是相对路径：沙箱内 agent 的主机名由平台注入，
		// 写死绝对地址在反代/换端口/多副本时全部失效。
		"baseUrl": gw.BasePath(),
	})
}

// llmProxy OpenAI 兼容透传。
//
// 认证不走 authed：那条路径认的是平台 JWT/API Key，这里认的是虚拟 key，
// 两者用同一个签名密钥但靠 scope 区分（见 auth.VerifyVirtualKey 的注释）。
func (h *handler) llmProxy(w http.ResponseWriter, r *http.Request) {
	gw := llmGateway()
	if gw == nil {
		fail(w, http.StatusNotImplemented, "LLM 网关未启用（需要以 -llm-gateway 与 -llm-key 启动）")
		return
	}
	svc, ok := h.cfg.Auth.(*auth.Service)
	if !ok {
		fail(w, http.StatusNotImplemented, "当前认证实现不支持校验虚拟 key（需要启用 auth.Service）")
		return
	}
	if h.cfg.Admin == nil {
		// 没有 AdminStore 也能转发，但记账会全丢——那等于悄悄免费。
		// 宁可不提供服务，也不给出「以为在计费其实没记」的假象。
		fail(w, http.StatusNotImplemented, "治理接口未启用（需要 -store sqlite 或 postgres）")
		return
	}

	claims, err := svc.VerifyVirtualKey(bearerToken(r))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrExpired):
			fail(w, http.StatusUnauthorized, "LLM 令牌已过期，请重新签发")
		default:
			fail(w, http.StatusUnauthorized, "LLM 令牌无效")
		}
		return
	}

	tenant, err := h.cfg.Admin.GetTenant(r.Context(), claims.TenantID)
	if err != nil {
		// 令牌指向的租户查不到（已删/换了库）：对调用方来说就是身份无效。
		fail(w, http.StatusUnauthorized, "LLM 令牌对应的租户不存在")
		return
	}
	if tenant.Status == store.StatusSuspended {
		fail(w, http.StatusForbidden, "租户已停用")
		return
	}

	// 额度检查必须在转发之前。设计文档说的是「先做完再拒绝」，那说的是 run 级别；
	// 网关是唯一能在钱花出去之前拦住的地方，所以这里就是那道闸。
	if limit := tenant.MonthlyTokenLimit; limit > 0 {
		now := time.Now().UTC()
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		sum, err := h.cfg.Admin.SumUsage(r.Context(), store.UsageFilter{
			TenantID: claims.TenantID,
			From:     monthStart,
			Limit:    -1, // 额度看整月总量，不能被分页截断
		})
		if err != nil {
			h.failErr(w, err)
			return
		}
		if used := sum.InputTokens + sum.OutputTokens; used >= limit {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":       "本月 token 额度已用尽，请联系管理员调整额度",
				"code":        "quota_exceeded",
				"usedTokens":  used,
				"limitTokens": limit,
				"resetsAt":    monthStart.AddDate(0, 1, 0),
			})
			return
		}
	}

	// 读一遍请求体只为拿 model：模型名决定 provider 与单价。
	// 读完立刻放回 r.Body，保证转发给上游的还是原始字节。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLLMBodyBytes))
	if err != nil {
		fail(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var shape struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &shape)
	model := strings.TrimSpace(shape.Model)

	meta := llm.Meta{
		TenantID:  claims.TenantID,
		UserID:    claims.UserID,
		SessionID: claims.SessionID,
		Provider:  gw.ProviderFor(model),
		Model:     model,
	}

	// 记账上下文必须与客户端请求解绑：客户端提前断开时 r.Context() 已经取消，
	// 用它写库会直接失败——可那一轮的钱确实花了，账必须在。
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()

	err = gw.ServeWith(r.Context(), w, r, meta, func(model string, in, out int64) {
		cost := gw.Cost(model, in, out)
		h.recordUsage(recCtx, r, claims, meta.Provider, model, in, out, cost)
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, llm.ErrBadRequest):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, llm.ErrUnknownProvider), errors.Is(err, llm.ErrNoAPIKey):
		// 配置问题，但对调用方而言是「这个模型现在不可用」，且不该暴露内部细节。
		h.log.Printf("LLM 网关配置问题: %v", err)
		fail(w, http.StatusBadGateway, "上游模型服务未配置")
	default:
		h.log.Printf("LLM 转发失败: %v", err)
		fail(w, http.StatusBadGateway, "上游模型服务请求失败")
	}
}

// recordUsage 落一条用量 + 一条审计。
//
// 单独拆出来是因为它可能在「客户端已经断开」之后才被调用，此时绝对不能
// 依赖请求上下文；ctx 由调用方给一个解绑过的、带超时的。
func (h *handler) recordUsage(ctx context.Context, r *http.Request, claims auth.VirtualClaims, provider, model string, in, out, cost int64) {
	id, err := auth.RandomID()
	if err != nil {
		// 记账失败不能影响已经发给客户端的响应（此时响应早就写完了），
		// 但必须留痕：金额错了要能查到是「没记上」而不是「用了没收费」。
		h.log.Printf("生成 usage ID 失败: %v", err)
	}
	u := store.Usage{
		ID:           id,
		TenantID:     claims.TenantID,
		UserID:       claims.UserID,
		SessionID:    claims.SessionID,
		Provider:     provider,
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		CostMicroUSD: cost,
		// 只有网关看到的数才参与计费（设计文档 D4）。agent 自报的用量将来若能拿到，
		// 要记成 source=agent，仅供展示。
		Source:    "gateway",
		CreatedAt: time.Now().UTC(),
	}
	if err := h.cfg.Admin.InsertUsage(ctx, u); err != nil {
		h.log.Printf("写入 usage 失败 tenant=%s model=%s in=%d out=%d: %v",
			claims.TenantID, model, in, out, err)
	}
	// 落库与打点是两条独立的路：库写失败时指标仍然要涨（它是排障时唯一
	// 「有没有流量」的凭据），指标库挂掉时也不能拖累记账。所以不放进同一个 if。
	obs.RecordLLMUsage(model, in, out, cost)
	// 审计用解绑后的上下文：h.audit 内部读 r.Context()，这里换成可用的那个。
	h.audit(r.WithContext(ctx), "llm.completion", "session", claims.SessionID, map[string]any{
		"model":        model,
		"provider":     provider,
		"inputTokens":  in,
		"outputTokens": out,
		"costMicroUsd": cost,
	})
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
