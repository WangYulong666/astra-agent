package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenResponse 覆盖三家返回字段的并集。
// github 用逗号分隔 scope，google 用空格，notion 不给 scope——解析时都兼容。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// AuthorizeURL 生成 OAuth 授权地址。
//
// APIKey 型返回空串（上层据此回 needs_key=true 走 PutKey）。
// state 为空时自己签一个：handler 目前不传 state，但签名必须由本包掌握，
// 不能信任调用方随便塞一个字符串（那样回调就校验不了）。
func (m *manager) AuthorizeURL(ctx context.Context, tenantID, userID, name, redirectURI, state string) (string, error) {
	if !m.Enabled() {
		return "", ErrDisabled
	}
	def, ok := lookup(name)
	if !ok {
		return "", fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
	}
	if def.Kind == KindAPIKey {
		return "", nil
	}
	id, secret := m.clientCreds(name)
	if id == "" || secret == "" {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, name)
	}

	if strings.TrimSpace(state) == "" {
		signed, err := m.signState(tenantID, userID, name, time.Now())
		if err != nil {
			return "", err
		}
		state = signed
	} else if _, _, _, err := m.ParseState(state); err != nil {
		// 调用方给的 state 过不了自己的校验就直接拒绝，别把坏 state 塞进授权页。
		return "", err
	}

	u, err := url.Parse(def.authorizeURL)
	if err != nil {
		return "", fmt.Errorf("connector: %s 授权地址非法: %w", name, err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", id)
	q.Set("state", state)
	if redirect := m.redirectURI(redirectURI); redirect != "" {
		q.Set("redirect_uri", redirect)
	}
	// notion 不接受 scope 参数（权限靠授权页勾选页面），def.Scopes 为空即跳过。
	if len(def.Scopes) > 0 {
		q.Set("scope", strings.Join(def.Scopes, " "))
	}
	for k, v := range def.extraAuth {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange 用回调的 code 换 token 并存入 vault。
//
// 注意 redirect_uri 必须和授权时用的完全一致（差一个斜杠都会被拒），
// 所以两个入口都走 redirectURI()。
func (m *manager) Exchange(ctx context.Context, tenantID, userID, name, code, redirectURI string) (Credential, error) {
	if !m.Enabled() {
		return Credential{}, ErrDisabled
	}
	def, ok := lookup(name)
	if !ok {
		return Credential{}, fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
	}
	if def.Kind != KindOAuth2 {
		return Credential{}, fmt.Errorf("connector: %s 是 API key 型，请用 /key 提交密钥", name)
	}
	id, secret := m.clientCreds(name)
	if id == "" || secret == "" {
		return Credential{}, fmt.Errorf("%w: %s", ErrUnsupported, name)
	}
	if strings.TrimSpace(code) == "" {
		return Credential{}, errors.New("connector: 回调 code 为空")
	}

	params := url.Values{"code": {code}}
	if redirect := m.redirectURI(redirectURI); redirect != "" {
		params.Set("redirect_uri", redirect)
	}
	switch def.tokenStyle {
	case tokenStyleNotion:
		// notion：grant_type 必填，凭据走 Basic auth，redirect_uri 只在授权时带过才需要。
		params.Set("grant_type", "authorization_code")
	default:
		if def.needsGrantType {
			params.Set("grant_type", "authorization_code")
		}
		params.Set("client_id", id)
		params.Set("client_secret", secret)
	}

	tr, err := m.requestToken(ctx, def, params)
	if err != nil {
		return Credential{}, err
	}

	now := time.Now().UTC()
	cred := Credential{
		TenantID: tenantID,
		UserID:   userID,
		Name:     name,
		Kind:     def.Kind,
		Token:    tr.AccessToken,
		Refresh:  tr.RefreshToken,
		Scopes:   parseScopes(tr.Scope),
		LinkedAt: now,
	}
	if tr.ExpiresIn > 0 {
		cred.Expires = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if len(cred.Scopes) == 0 {
		// 提供方不回 scope（notion、部分 github 响应）时按申请时的最小集记录。
		cred.Scopes = append([]string(nil), def.Scopes...)
	}
	if err := m.vault.set(cred); err != nil {
		return Credential{}, err
	}
	m.invalidateTokenCache(tenantID, userID, name)
	// 返回值不回 token：上层（回调页）只需要名字，凭据只该被使用、不该被读取。
	cred.Token = ""
	cred.Refresh = ""
	return cred, nil
}

// refresh 用 refresh_token 换新的一对 token 并落库。
func (m *manager) refresh(ctx context.Context, tenantID, userID, name string, cred Credential) (Credential, error) {
	def, ok := lookup(name)
	if !ok {
		return Credential{}, fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
	}
	if !def.refreshable {
		return Credential{}, fmt.Errorf("connector: %s 不支持刷新，请重新授权", name)
	}
	id, secret := m.clientCreds(name)
	if id == "" || secret == "" {
		return Credential{}, fmt.Errorf("%w: %s", ErrUnsupported, name)
	}

	params := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.Refresh},
	}
	if def.tokenStyle != tokenStyleNotion {
		params.Set("client_id", id)
		params.Set("client_secret", secret)
	}

	tr, err := m.requestToken(ctx, def, params)
	if err != nil {
		return Credential{}, fmt.Errorf("connector: 刷新 %s 的 token 失败（需重新授权）: %w", name, err)
	}
	now := time.Now().UTC()
	next := Credential{
		TenantID: tenantID,
		UserID:   userID,
		Name:     name,
		Kind:     cred.Kind,
		Token:    tr.AccessToken,
		Refresh:  tr.RefreshToken,
		Scopes:   cred.Scopes,
		LinkedAt: cred.LinkedAt,
	}
	if next.Refresh == "" {
		// 部分提供方刷新时不回新的 refresh_token，沿用旧的（github/google 常这样）。
		next.Refresh = cred.Refresh
	}
	if tr.ExpiresIn > 0 {
		next.Expires = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if next.LinkedAt.IsZero() {
		next.LinkedAt = now
	}
	if err := m.vault.set(next); err != nil {
		return Credential{}, err
	}
	// refresh 也可能由 Inject(env) 触发；让 TokenFor 缓存失效，避免继续用旧 token。
	m.invalidateTokenCache(tenantID, userID, name)
	return next, nil
}

// requestToken 发一次 token 请求。编码方式按连接器定：
//   - form：application/x-www-form-urlencoded，凭据在 body（github/google）
//   - notion：JSON body + HTTP Basic（官方文档明确要求）
func (m *manager) requestToken(ctx context.Context, def definition, params url.Values) (tokenResponse, error) {
	var tr tokenResponse
	endpoint := m.tokenURLs[def.Name]
	if endpoint == "" {
		endpoint = def.tokenURL
	}
	if endpoint == "" {
		return tr, fmt.Errorf("connector: %s 没有 token 端点", def.Name)
	}

	var (
		body    io.Reader
		ctype   string
		request *http.Request
		err     error
	)
	switch def.tokenStyle {
	case tokenStyleNotion:
		raw, marshalErr := json.Marshal(valuesToMap(params))
		if marshalErr != nil {
			return tr, fmt.Errorf("connector: 序列化 token 请求: %w", marshalErr)
		}
		body, ctype = bytes.NewReader(raw), "application/json"
	default:
		body, ctype = strings.NewReader(params.Encode()), "application/x-www-form-urlencoded"
	}

	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return tr, fmt.Errorf("connector: 构造 token 请求: %w", err)
	}
	request.Header.Set("Content-Type", ctype)
	request.Header.Set("Accept", "application/json")
	if def.tokenStyle == tokenStyleNotion {
		id, secret := m.clientCreds(def.Name)
		request.SetBasicAuth(id, secret)
	}

	resp, err := m.http.Do(request)
	if err != nil {
		return tr, fmt.Errorf("connector: 请求 %s 的 token 端点: %w", def.Name, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tr, fmt.Errorf("connector: 读 %s 的 token 响应: %w", def.Name, err)
	}
	if resp.StatusCode/100 != 2 {
		// 错误体里可能带 hint，但不含我们自己的密钥；截断避免把整页 HTML 记进日志。
		return tr, fmt.Errorf("connector: %s 换 token 失败（HTTP %d）: %s",
			def.Name, resp.StatusCode, clip(string(raw), 400))
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return tr, fmt.Errorf("connector: %s 的 token 响应不是合法 JSON: %w", def.Name, err)
	}
	if tr.Error != "" {
		return tr, fmt.Errorf("connector: %s 换 token 被拒: %s %s", def.Name, tr.Error, tr.ErrorDesc)
	}
	if tr.AccessToken == "" {
		return tr, fmt.Errorf("connector: %s 未返回 access_token", def.Name)
	}
	return tr, nil
}

func valuesToMap(v url.Values) map[string]string {
	out := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// parseScopes 兼容 github 的逗号分隔和 google 的空格分隔。
func parseScopes(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
