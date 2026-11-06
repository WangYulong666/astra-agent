package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/schedule"
	"github.com/WangYulong666/astra-agent/internal/store"
)

func (h *handler) schedulesDisabled(w http.ResponseWriter) bool {
	if h.cfg.Schedules == nil || h.cfg.Scheduler == nil || !h.cfg.Scheduler.Enabled() {
		fail(w, http.StatusServiceUnavailable, "定时任务未启用：请以 -scheduler 启动后重启")
		return true
	}
	return false
}

func (h *handler) listSchedules(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	p := h.principal(r)
	items, err := h.cfg.Schedules.List(r.Context(), p.TenantID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	// 成员只能看到自己的；owner/admin 看全租户
	if !p.Can("tenant:manage") {
		kept := items[:0]
		for _, s := range items {
			if s.UserID == p.UserID || p.Can("user:manage") {
				kept = append(kept, s)
			}
		}
		items = kept
	}
	if items == nil {
		items = []schedule.Schedule{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) createSchedule(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	p := h.principal(r)
	var req struct {
		Name      string           `json:"name"`
		SessionID string           `json:"session_id"`
		Trigger   schedule.Trigger `json:"trigger"`
		Cron      string           `json:"cron"`
		Timezone  string           `json:"timezone"`
		Prompt    string           `json:"prompt"`
		Enabled   *bool            `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		fail(w, http.StatusBadRequest, "prompt 不能为空")
		return
	}
	if req.SessionID == "" {
		fail(w, http.StatusBadRequest, "session_id 必填：定时任务复用一个会话的工作区")
		return
	}
	// 会话必须属于调用者，否则等于允许往别人会话里定时注入指令。
	// 校验的是 body 里的 session_id（路由可能挂在 /v1/schedules 上，
	// 也可能挂在 /v1/sessions/{id}/schedules 上，按 body 判断两种情况都对）。
	sess, err := h.cfg.Manager.GetSession(r.Context(), req.SessionID)
	if err != nil {
		h.failErr(w, err)
		return
	}
	if !h.canSee(h.principal(r), sess) {
		fail(w, http.StatusNotFound, "会话不存在")
		return
	}
	if req.Trigger == "" {
		req.Trigger = schedule.TriggerCron
	}
	switch req.Trigger {
	case schedule.TriggerCron:
		if strings.TrimSpace(req.Cron) == "" {
			fail(w, http.StatusBadRequest, "trigger=cron 时 cron 必填（5 段：分 时 日 月 周）")
			return
		}
	case schedule.TriggerWebhook:
		// token 由存储层生成
	default:
		fail(w, http.StatusBadRequest, "trigger 只能是 cron 或 webhook")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := time.Now().UTC()
	s := schedule.Schedule{
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		SessionID: req.SessionID,
		Name:      req.Name,
		Trigger:   req.Trigger,
		Cron:      req.Cron,
		Timezone:  req.Timezone,
		Prompt:    req.Prompt,
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	created, err := h.cfg.Schedules.Create(r.Context(), s)
	if err != nil {
		h.scheduleFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	s, ok := h.ownedSchedule(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *handler) patchSchedule(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	s, ok := h.ownedSchedule(w, r)
	if !ok {
		return
	}
	var req struct {
		Name     *string `json:"name"`
		Cron     *string `json:"cron"`
		Timezone *string `json:"timezone"`
		Prompt   *string `json:"prompt"`
		Enabled  *bool   `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if req.Name != nil {
		s.Name = strings.TrimSpace(*req.Name)
	}
	if req.Cron != nil {
		s.Cron = strings.TrimSpace(*req.Cron)
	}
	if req.Timezone != nil {
		s.Timezone = *req.Timezone
	}
	if req.Prompt != nil {
		if strings.TrimSpace(*req.Prompt) == "" {
			fail(w, http.StatusBadRequest, "prompt 不能为空")
			return
		}
		s.Prompt = *req.Prompt
	}
	if req.Enabled != nil {
		s.Enabled = *req.Enabled
	}
	s.UpdatedAt = time.Now().UTC()
	if err := h.cfg.Schedules.Update(r.Context(), s); err != nil {
		h.scheduleFail(w, err)
		return
	}
	// 存储层可能重算了 NextRunAt（改 cron / 改时区 / 重新启用），
	// 回读一次再吐给客户端，否则前端拿到的是旧的下次运行时间。
	if fresh, err := h.cfg.Schedules.Get(r.Context(), s.ID); err == nil {
		s = fresh
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *handler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	s, ok := h.ownedSchedule(w, r)
	if !ok {
		return
	}
	if err := h.cfg.Schedules.Delete(r.Context(), s.TenantID, s.ID); err != nil {
		h.scheduleFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	s, ok := h.ownedSchedule(w, r)
	if !ok {
		return
	}
	runID, err := h.cfg.Scheduler.RunNow(r.Context(), s.ID)
	if err != nil {
		h.scheduleFail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

// hookTrigger 是 webhook 入口：**不需要登录**，靠 URL 里的不可猜 token 鉴权。
//
// 这样设计是因为调用方通常是外部系统（GitHub webhook、监控告警），
// 它们没法拿平台的 JWT。token 泄露等同于「能往这个会话里发指令」，
// 所以 token 必须足够长、可轮换，且只绑一个会话。
func (h *handler) hookTrigger(w http.ResponseWriter, r *http.Request) {
	if h.schedulesDisabled(w) {
		return
	}
	token := r.PathValue("token")
	if token == "" {
		fail(w, http.StatusNotFound, "hook 不存在")
		return
	}
	s, err := h.cfg.Schedules.GetByHookToken(r.Context(), token)
	if err != nil {
		// 刻意不区分「token 错」和「不存在」：不给枚举口子
		fail(w, http.StatusNotFound, "hook 不存在")
		return
	}
	if !s.Enabled {
		fail(w, http.StatusConflict, "该 hook 已停用")
		return
	}
	// 请求体作为额外上下文（可选）：非空时拼在指令后面，方便传 payload 摘要
	extra := ""
	if r.Body != nil {
		if body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10)); len(body) > 0 {
			extra = string(body)
		}
	}
	runID, err := h.runSchedule(r.Context(), s.ID, extra)
	if err != nil {
		h.scheduleFail(w, err)
		return
	}
	h.audit(r, "schedule.hook", "schedule", s.ID, map[string]any{"run_id": runID})
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}

// runSchedule 触发一次。webhook 带载荷时走 PromptRunner 扩展接口：
// Scheduler 接口（冻结）上的 RunNow 没有载荷入参，载荷只能从这条旁路进。
// 载荷只对这一次生效，不写回 schedule 的 prompt 模板。
func (h *handler) runSchedule(ctx context.Context, id, extra string) (string, error) {
	if extra != "" {
		if pr, ok := h.cfg.Scheduler.(schedule.PromptRunner); ok {
			return pr.RunNowWithPrompt(ctx, id, extra)
		}
	}
	return h.cfg.Scheduler.RunNow(ctx, id)
}

func (h *handler) ownedSchedule(w http.ResponseWriter, r *http.Request) (schedule.Schedule, bool) {
	p := h.principal(r)
	s, err := h.cfg.Schedules.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.scheduleFail(w, err)
		return schedule.Schedule{}, false
	}
	if s.TenantID != p.TenantID || (s.UserID != p.UserID && !p.Can("user:manage")) {
		// 404 而不是 403：不暴露「存在但不属于你」
		fail(w, http.StatusNotFound, "定时任务不存在")
		return schedule.Schedule{}, false
	}
	return s, true
}

func (h *handler) scheduleFail(w http.ResponseWriter, err error) {
	if errors.Is(err, schedule.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "定时任务不存在")
		return
	}
	// 调用方写错了（cron 表达式非法、缺字段）：400，不要走 500。
	if errors.Is(err, schedule.ErrInvalid) || errors.Is(err, schedule.ErrNoNext) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	// 配额满了属于「当前状态冲突」，409 比 500 有意义。
	if errors.Is(err, schedule.ErrQuota) {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	h.failErr(w, err)
}
