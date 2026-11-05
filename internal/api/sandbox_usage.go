package api

// 沙箱管理面与用量查询。
//
// 归属：T3 实现。
//
// 这两块放在一个文件里，是因为它们共享同一个前提：数据本身不带租户，
// 归属只能靠会话反查出来。沙箱来自 provider（它只认 ID），用量来自网关记账
// （它只认 tenant/user 字段）。一旦反查这一步写错，泄漏的就是整租户的资源清单。

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/sandbox"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// sandboxView 是对外的沙箱条目：provider 的 Info + 反查出来的会话归属。
//
// 刻意不整包返回 sandbox.Info：它的 Endpoint 里有 acp-proxy 的 Bearer token，
// 那是一个能直接连进沙箱执行命令的凭据，绝不能出现在列表接口里。
type sandboxView struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	State        string    `json:"state"`
	Workspace    string    `json:"workspace,omitempty"`
	PID          int       `json:"pid,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	SessionID    string    `json:"sessionId,omitempty"`
	SessionTitle string    `json:"sessionTitle,omitempty"`
	TenantID     string    `json:"tenantId,omitempty"`
	UserID       string    `json:"userId,omitempty"`
}

// listSandboxes 把「provider 眼里的沙箱」和「store 眼里的会话」拼起来。
func (h *handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	if h.cfg.Provider == nil {
		fail(w, http.StatusNotImplemented, "未配置沙箱 provider")
		return
	}
	p := h.principal(r)
	infos, err := h.cfg.Provider.List(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	sessions, err := h.cfg.Store.ListSessions(r.Context())
	if err != nil {
		h.failErr(w, err)
		return
	}
	bySandbox := make(map[string]store.Session, len(sessions))
	for _, s := range sessions {
		id := s.SandboxID
		if id == "" {
			// 历史数据里 SandboxID 可能与 ID 相同也可能缺失，两种都当别名收进来。
			id = s.ID
		}
		bySandbox[id] = s
	}

	out := make([]sandboxView, 0, len(infos))
	for _, inf := range infos {
		sess, linked := bySandbox[inf.ID]
		if linked && !h.canSee(p, sess) {
			continue
		}
		if !linked && p.Via != auth.ViaDev {
			// 找不到归属会话的沙箱（会话已删、或换了 provider 留下的残留）不给普通租户看：
			// 没有归属信息就无法证明它属于谁，展示出来等于把「本机还有个别人的沙箱」
			// 告诉所有人。代价是这类残留只能由运维用 CLI 清——比泄漏便宜。
			continue
		}
		out = append(out, sandboxView{
			ID:           inf.ID,
			Provider:     inf.Provider,
			State:        inf.State.String(),
			Workspace:    inf.Workspace,
			PID:          inf.PID,
			CreatedAt:    inf.CreatedAt,
			SessionID:    sess.ID,
			SessionTitle: sess.Title,
			TenantID:     sess.TenantID,
			UserID:       sess.UserID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteSandbox 强杀沙箱。
//
// 「强杀」只销毁运行环境，**不删会话记录**：沙箱是资源，会话是数据。
// 一个卡死的沙箱需要立刻干掉（它可能正在烧 CPU 或占着端口），
// 但用户的历史对话、审批记录不该因为「进程被杀了」而一起消失——
// 而且大多数强杀恰恰是因为 agent 出问题，这时候历史记录才是最该保住的证据。
// 会话记录留给 DELETE /v1/sessions/{id}，那是一条用户主动的、不可逆的数据删除。
func (h *handler) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	if h.cfg.Provider == nil {
		fail(w, http.StatusNotImplemented, "未配置沙箱 provider")
		return
	}
	p := h.principal(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		fail(w, http.StatusNotFound, "沙箱不存在")
		return
	}

	sess, linked, err := h.sandboxSession(r.Context(), id)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if linked && !h.canSee(p, sess) {
		fail(w, http.StatusNotFound, "沙箱不存在")
		return
	}
	if !linked && p.Via != auth.ViaDev {
		// 无归属的沙箱对普通租户不可见（见 listSandboxes 的同款注释），
		// 这里也必须是 404，否则「猜 ID 能不能删」就成了一个探测手段。
		fail(w, http.StatusNotFound, "沙箱不存在")
		return
	}

	if err := h.cfg.Provider.Destroy(r.Context(), id); err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			fail(w, http.StatusNotFound, "沙箱不存在")
			return
		}
		h.failErr(w, err)
		return
	}
	h.audit(r, "sandbox.destroy", "sandbox", id, map[string]string{"sessionId": sess.ID})
	w.WriteHeader(http.StatusNoContent)
}

// sandboxSession 反查沙箱归属的会话。
//
// 返回 linked=false 不是错误：provider 里有、store 里没有，是清理残留时的正常态。
// 调用方必须把它当「无归属」，而不是当「公共的」。
func (h *handler) sandboxSession(ctx context.Context, sandboxID string) (store.Session, bool, error) {
	sessions, err := h.cfg.Store.ListSessions(ctx)
	if err != nil {
		return store.Session{}, false, err
	}
	for _, s := range sessions {
		if s.SandboxID == sandboxID || (s.SandboxID == "" && s.ID == sandboxID) {
			return s, true, nil
		}
	}
	return store.Session{}, false, nil
}

// ── 用量 ─────────────────────────────────────────────────────

// usageReport 是 /v1/usage 的响应：明细 + 汇总。
//
// 汇总跟明细一起回，是因为前端要同时画列表和顶部数字；分两个接口的话，
// 两次请求之间会有一条记账落库，页面上的合计与明细永远差一点点。
type usageReport struct {
	Items []store.Usage  `json:"items"`
	Sum   store.UsageSum `json:"sum"`
}

// listUsage 是租户内的用量查询。
//
// member 只能看自己：过滤条件用 Principal.UserID **覆盖**，而不是取 query 里的 userId。
// 信任 query 等于把「看谁的用量」交给客户端决定，一行 ?userId=xxx 就能翻别人的账。
func (h *handler) listUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	f, ok := h.usageFilter(w, r, p.TenantID)
	if !ok {
		return
	}
	if p.Role == store.RoleMember {
		f.UserID = p.UserID
	}
	h.writeUsage(w, r, f)
}

// adminUsage 是同租户的全局视图，支持 ?userId= 精确到人。
//
// 这个接口要 ActionUsageReadAll，member 进不来，所以 query 里的 userId
// 只在「本租户范围内」有意义——tenantID 仍然是 Principal 给的。
func (h *handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	f, ok := h.usageFilter(w, r, p.TenantID)
	if !ok {
		return
	}
	f.UserID = strings.TrimSpace(r.URL.Query().Get("userId"))
	h.writeUsage(w, r, f)
}

func (h *handler) writeUsage(w http.ResponseWriter, r *http.Request, f store.UsageFilter) {
	items, err := h.cfg.Admin.ListUsage(r.Context(), f)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if items == nil {
		items = []store.Usage{}
	}
	// 汇总不跟着分页走：Limit 只截明细，额度判断要看整段时间的账。
	sumFilter := f
	sumFilter.Limit = -1
	sum, err := h.cfg.Admin.SumUsage(r.Context(), sumFilter)
	if err != nil {
		h.failErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, usageReport{Items: items, Sum: sum})
}

// usageFilter 解析公共的过滤条件。tenantID 由调用方从 Principal 传入，这里不读任何请求字段。
func (h *handler) usageFilter(w http.ResponseWriter, r *http.Request, tenantID string) (store.UsageFilter, bool) {
	q := r.URL.Query()
	from, err := parseTimeParam(q.Get("from"))
	if err != nil {
		fail(w, http.StatusBadRequest, "from: "+err.Error())
		return store.UsageFilter{}, false
	}
	to, err := parseTimeParam(q.Get("to"))
	if err != nil {
		fail(w, http.StatusBadRequest, "to: "+err.Error())
		return store.UsageFilter{}, false
	}
	return store.UsageFilter{
		TenantID: tenantID,
		From:     from,
		To:       to,
		Limit:    parseLimit(r, 500, 5000),
	}, true
}

// modelBilling 是 billing 响应里的按模型小计：该模型的用量 + 计费时实际用的单价。
//
// 单价随账单一起回，是为了对账：CostMicroUSD 出了争议时，拿着
// tokens × 单价 直接就能复核，不用去翻网关当时的配置。
type modelBilling struct {
	Model        string `json:"model"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	Calls        int64  `json:"calls"`
	CostMicroUSD int64  `json:"costMicroUsd"`
	// 两个单价为空表示 LLM 网关未启用（此时本租户不会有网关记账，
	// 这几条用量来自别的来源，没有可信单价可给）。
	InputPerMTokMicroUSD  *int64 `json:"inputPerMTokMicroUSD,omitempty"`
	OutputPerMTokMicroUSD *int64 `json:"outputPerMTokMicroUSD,omitempty"`
}

// billing 回本月额度与已用量。
//
// 金额从 CostMicroUSD 转美元只在展示层做一次：库里用整数微美元是为了避免
// 浮点累加误差（几十万次调用的 1e-7 误差会变成账单上的几块钱），
// 一旦把 float 存回去，这个设计就白做了。
func (h *handler) billing(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminStore(w) {
		return
	}
	p := h.principal(r)
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	f := store.UsageFilter{TenantID: p.TenantID, From: monthStart, Limit: -1}
	if p.Role == store.RoleMember {
		f.UserID = p.UserID
	}
	sum, err := h.cfg.Admin.SumUsage(r.Context(), f)
	if err != nil {
		h.failErr(w, err)
		return
	}

	// 按模型小计。明细拉整月（Limit=-1）在内存里聚合：单机量级下成本远低于
	// 为它加一条 GROUP BY 查询，还顺带复用了两个 store 实现都支持的契约。
	items, err := h.cfg.Admin.ListUsage(r.Context(), f)
	if err != nil {
		h.failErr(w, err)
		return
	}
	order := []string{}
	byModel := map[string]*modelBilling{}
	for _, u := range items {
		mb, ok := byModel[u.Model]
		if !ok {
			mb = &modelBilling{Model: u.Model}
			byModel[u.Model] = mb
			order = append(order, u.Model)
		}
		mb.InputTokens += u.InputTokens
		mb.OutputTokens += u.OutputTokens
		mb.CostMicroUSD += u.CostMicroUSD
		mb.Calls++
	}

	// 单价以网关当前价格表为准：它就是记账时用的那张表（Pricing.Price
	// 未命中会返回兜底价，与网关算钱的行为一致）。
	gw := llmGateway()
	models := make([]modelBilling, 0, len(order))
	for _, name := range order {
		mb := *byModel[name]
		if gw != nil && gw.Pricing != nil {
			if pr, _ := gw.Pricing.Price(mb.Model); pr.InputPerMTokMicroUSD > 0 || pr.OutputPerMTokMicroUSD > 0 {
				in, out := pr.InputPerMTokMicroUSD, pr.OutputPerMTokMicroUSD
				mb.InputPerMTokMicroUSD = &in
				mb.OutputPerMTokMicroUSD = &out
			}
		}
		models = append(models, mb)
	}

	var limit int64
	if p.TenantID != "" {
		t, err := h.cfg.Admin.GetTenant(r.Context(), p.TenantID)
		if err != nil {
			h.failErr(w, err)
			return
		}
		limit = t.MonthlyTokenLimit
	}

	total := sum.InputTokens + sum.OutputTokens
	var usedPercent float64
	if limit > 0 {
		usedPercent = math.Round(float64(total)/float64(limit)*10000) / 100 // 两位小数
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"inputTokens":  sum.InputTokens,
		"outputTokens": sum.OutputTokens,
		// 展示保留 4 位：单次调用的成本常在 $0.0001 量级，2 位会让整页都是 0.00。
		"costUsd":           math.Round(float64(sum.CostMicroUSD)/1e6*10000) / 10000,
		"calls":             sum.Calls,
		"monthlyTokenLimit": limit,
		"usedPercent":       usedPercent,
		// 超限判断用 >=：额度是「最多用到这个数」，用满即视为不能再来。
		"overLimit": limit > 0 && total >= limit,
		// 按模型小计 + 计费单价，见 modelBilling 的注释。
		"models": models,
	})
}
