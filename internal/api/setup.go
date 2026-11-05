package api

// 首启向导与 LLM 配置的端点。
//
// 为什么单独一个文件：setup 的访问模式很特殊——**必须在没有任何身份时可达**
// （登录页都进不去的地方，向导就是唯一的门），又必须严守「只服务空系统」的
// 边界，跟普通治理端点的「先鉴权再干活」是两套逻辑，混进 platform_routes
// 会让那条「谁能调什么」的规则线变模糊。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/llm"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// LLMSettingsStore 由 internal/app 实现：内存热更 + 落盘持久化。
// HTTP 层只认这个接口——它不该知道 key 被写到了哪个文件。
type LLMSettingsStore interface {
	// Current 返回当前配置。GatewayOn=false 表示装配时没开网关（CLI 未加 flag），
	// 此时 Set 应返回错误，而不是悄悄改一份没人用的配置。
	Current() (apiKey, baseURL string, gatewayOn bool)
	// Set 热更上游 key/baseURL 并持久化。key 允许为空串表示清除。
	Set(apiKey, baseURL string) error
}

// registerSetup 注册公开的向导端点。在 Handler 的路由装配处调用。
func (h *handler) registerSetup(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/setup/state", h.setupState)
	mux.HandleFunc("POST /v1/setup/complete", h.setupComplete)

	// 管理端：查看/热更上游凭据。挂在治理路由之后注册，需要登录 + owner。
	mux.HandleFunc("GET /v1/admin/llm-settings", h.authed(h.getLLMSettings, auth.ActionTenantManage))
	mux.HandleFunc("POST /v1/admin/llm-settings", h.authed(h.setLLMSettings, auth.ActionTenantManage))
	mux.HandleFunc("POST /v1/admin/llm-settings/test", h.authed(h.testLLMSettings, auth.ActionTenantManage))
}

// setupState 是向导的第一步：系统现在什么都没有，还是已经能用了。
// 公开可读——它只暴露「有没有租户」这一个布尔和 agent 展示名，
// 泄露面可控；不公开的话前端在登录前无从判断该展示向导还是登录框。
func (h *handler) setupState(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"agentName": h.cfg.Info["agentName"],
		"approval":  h.cfg.Info["approval"],
	}
	needs, err := h.noTenants(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out["needsSetup"] = needs

	if h.cfg.LLMSettings != nil {
		_, _, on := h.cfg.LLMSettings.Current()
		out["llmGateway"] = on
		_, _, hasKey := h.llmKeyState()
		out["llmConfigured"] = hasKey
	} else {
		out["llmGateway"] = false
		out["llmConfigured"] = false
	}
	writeJSON(w, http.StatusOK, out)
}

// llmKeyState 当前网关是否有 key 可用。
func (h *handler) llmKeyState() (key, base string, ok bool) {
	if h.cfg.LLMSettings == nil {
		return "", "", false
	}
	key, base, on := h.cfg.LLMSettings.Current()
	return key, base, on && key != ""
}

// noTenants 系统里是否一个租户都没有。
func (h *handler) noTenants(ctx context.Context) (bool, error) {
	if h.cfg.Admin == nil {
		return false, nil // file 后端：没有治理层，谈不上向导
	}
	tenants, err := h.cfg.Admin.ListTenants(ctx)
	if err != nil {
		return false, err
	}
	return len(tenants) == 0, nil
}

// setupComplete 向导的收口：建租户、建管理员、**直接返回登录 token**。
//
// 与 /v1/auth/bootstrap 的差别只有两个：密码由用户当场指定（bootstrap 是
// 服务端随机生成——那个流程在 .app 里用户根本看不到密码）；以及成功后
// 顺手签发 token，让向导能无缝跳进下一步，不再要求用户手输一遍刚设的密码。
//
// 安全边界：仅当系统里没有租户时可用。有人想对已初始化的系统调这个接口
// 建管理员，得到的会是 409——「第一次」这个窗口只在真正第一次打开。
func (h *handler) setupComplete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Admin == nil {
		fail(w, http.StatusPreconditionFailed, "当前存储后端不支持初始化向导")
		return
	}
	needs, err := h.noTenants(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !needs {
		fail(w, http.StatusConflict, "系统已初始化，请直接登录")
		return
	}

	var req struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Slug = strings.ToLower(strings.TrimSpace(req.Slug))
	if req.Slug == "" {
		req.Slug = "default"
	}
	if req.Name == "" {
		req.Name = "默认租户"
	}
	// 邮箱/密码格式由 auth service 与 store 的既有校验兜底（CreateUser 会查重）。
	if req.Email == "" || len(req.Password) < 8 {
		fail(w, http.StatusBadRequest, "需要 email 与至少 8 位的 password")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	tid, err := auth.RandomID()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	uid, err := auth.RandomID()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now().UTC()
	tenant := store.Tenant{ID: tid, Name: req.Name, Slug: req.Slug, Status: store.StatusActive, CreatedAt: now}
	if err := h.cfg.Admin.CreateTenant(ctx, tenant); err != nil {
		fail(w, http.StatusConflict, "租户创建失败（slug 可能已被占用）: "+err.Error())
		return
	}
	owner := store.User{
		ID: uid, TenantID: tid, Email: req.Email, DisplayName: "管理员",
		PasswordHash: hash, Role: store.RoleOwner, Status: store.StatusActive, CreatedAt: now,
	}
	if err := h.cfg.Admin.CreateUser(ctx, owner); err != nil {
		// 无法回滚租户（AdminStore 没有 DeleteTenant）：到这一步还失败基本只剩
		// 存储层故障，那种情况下整个系统本来就不健康。slug/email 冲突已在上面拦住。
		fail(w, http.StatusConflict, "管理员创建失败（email 可能已被占用）")
		return
	}
	h.audit(r, "tenant.setup", "tenant", tid, map[string]string{"slug": req.Slug, "via": "setup-wizard"})

	token, err := h.loginToken(req.Slug, req.Email, req.Password)
	if err != nil {
		// 账号已建好，只是自动登录失败：让用户去登录页手输，不算向导失败。
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": tenant, "user": owner, "autoLogin": false})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant": tenant, "user": owner, "autoLogin": true, "token": token,
	})
}

// loginToken 用刚创建的凭据走一遍真实登录并签发 token。
// 走 Login 而不是直接手工拼 User：让「刚建的账号能登录」这件事本身被验证到。
func (h *handler) loginToken(slug, email, password string) (string, error) {
	u, err := h.cfg.Auth.Login(context.Background(), slug, email, password)
	if err != nil {
		return "", err
	}
	if svc, ok := h.cfg.Auth.(*auth.Service); ok {
		token, _, _, _, err := svc.IssuePair(u)
		return token, err
	}
	token, _, err := h.cfg.Auth.Issue(u)
	return token, err
}

// getLLMSettings 查看上游凭据状态。**永远不回 key 本体**，只回有没有。
func (h *handler) getLLMSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.LLMSettings == nil {
		writeJSON(w, http.StatusOK, map[string]any{"gateway": false})
		return
	}
	key, base, on := h.cfg.LLMSettings.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"gateway": on, "hasKey": key != "", "baseUrl": base,
	})
}

// setLLMSettings 热更上游凭据。key 为空串=清除（回到「网关在但 502」的状态），
// 前端要不要允许清，是它的事；后端不拦——排障时「把 key 摘掉看报错变化」是合法操作。
func (h *handler) setLLMSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.LLMSettings == nil {
		fail(w, http.StatusPreconditionFailed, "LLM 网关未启用（启动时未开 llm-gateway）")
		return
	}
	var req struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseUrl"`
		// KeepKey=true 时 apiKey 只允许为空：保留现有 key、只更新 baseUrl。
		// 没有它，「只改 baseUrl」这个合法操作做不到——GET 永不回 key 本体，
		// 前端没法把旧 key 原样发回来。清除 key 用显式 clear，别用空串猜。
		KeepKey bool `json:"keepKey"`
		Clear   bool `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	if req.KeepKey && req.APIKey != "" {
		fail(w, http.StatusBadRequest, "keepKey=true 时不要传 apiKey")
		return
	}
	if req.Clear && req.APIKey != "" {
		fail(w, http.StatusBadRequest, "clear=true 时不要传 apiKey")
		return
	}
	if req.KeepKey || req.Clear {
		cur, base, on := h.cfg.LLMSettings.Current()
		if !on {
			fail(w, http.StatusPreconditionFailed, "LLM 网关未启用")
			return
		}
		if req.KeepKey {
			req.APIKey = cur
			if req.BaseURL == "" {
				req.BaseURL = base
			}
		} else {
			req.APIKey = ""
		}
	}
	if err := h.cfg.LLMSettings.Set(req.APIKey, req.BaseURL); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.audit(r, "llm.settings.update", "settings", "llm", map[string]any{
		"hasKey": req.APIKey != "", "baseUrl": req.BaseURL,
	})
	key, base, _ := h.cfg.LLMSettings.Current()
	writeJSON(w, http.StatusOK, map[string]any{"hasKey": key != "", "baseUrl": base})
}

// testLLMSettings 用给定的 key/baseUrl 真连一次上游，max_tokens=1。
// 必须真连：向导里「测试通过」如果只是格式校验，用户填错 key 会在
// 第一个任务上才发现，那时候他只会认为「agent 坏了」而不是「key 填错了」。
func (h *handler) testLLMSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	// 不带 key 时测当前已配置的——用于「系统里存的那把还好使吗」。
	if req.APIKey == "" && h.cfg.LLMSettings != nil {
		req.APIKey, req.BaseURL, _ = h.cfg.LLMSettings.Current()
	}
	if req.APIKey == "" {
		fail(w, http.StatusBadRequest, "缺少 apiKey")
		return
	}
	if req.BaseURL == "" {
		req.BaseURL = llm.DefaultDeepSeekBaseURL
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	latency, err := probeUpstream(ctx, req.BaseURL, req.APIKey)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latencyMs": latency.Milliseconds()})
}

// probeUpstream 对上游发一次最小补全。返回耗时；任何非 200 都带原因返回。
func probeUpstream(ctx context.Context, baseURL, key string) (time.Duration, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      "deepseek-chat",
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	url := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, fmt.Errorf("连不上上游: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return latency, errors.New("key 被上游拒绝（401/403），请检查 key 是否有效")
	}
	if resp.StatusCode != http.StatusOK {
		return latency, fmt.Errorf("上游返回 %s", resp.Status)
	}
	return latency, nil
}
