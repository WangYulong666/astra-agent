// Package client 是 astra-cli / admin-cli 共用的控制面 HTTP 客户端。
//
// 设计约束（来自 platform-extension.md 第三节）：
//   - 认证只有一种方式：Authorization: Bearer <token>；
//   - 错误只有一种形状：*APIError，前端不需要按状态码分支猜语义；
//   - 事件流与普通请求的超时策略相反：普通请求 30s，SSE 不设总超时（靠 ctx）。
//
// 为什么把客户端抽成独立包而不是各 CLI 各写一份：两个 CLI 打的是同一套接口，
// 两份实现必然漂移——一边加了重试、另一边没加，最后「同一件事两个工具表现不同」。
package client

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

const (
	// DefaultTimeout 普通请求的超时。SSE 不走这个值，见 streamDo。
	DefaultTimeout = 30 * time.Second
	// maxErrorBody 错误响应最多读这么多，避免被一个异常的服务端撑爆内存。
	maxErrorBody = 64 << 10
	userAgent    = "astra-cli/1.0"
)

// Client 是控制面客户端。零值不可用，必须经 New 构造。
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// logf 由 SetLogger 注入，-v 时打印每次请求。默认 nil（静默）。
	logf func(format string, args ...any)
}

// New 构造客户端。baseURL 允许带或不带结尾斜杠。
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		HTTP:    &http.Client{Timeout: DefaultTimeout},
	}
}

// SetLogger 打开请求级日志（CLI 的 -v）。nil 表示关闭。
func (c *Client) SetLogger(fn func(format string, args ...any)) { c.logf = fn }

func (c *Client) logfReq(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// APIError 是服务端返回的非 2xx。Message 直接取响应体里的 error 字段
// （服务端文案本来就是给人看的，再包一层只会丢掉「哪个参数错了」这类信息）。
type APIError struct {
	Status  int    `json:"-"`
	Message string `json:"error"`
	// Method/Path 便于 -v 或脚本定位是哪一次调用失败的。
	Method string `json:"-"`
	Path   string `json:"-"`
}

// Error 输出人话。状态码单独分支，是因为这几种情况用户要做的事完全不同：
// 401 去重新登录、403 去要权限、404 是 id 错了、503 是运维没开能力。
func (e *APIError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return msg + "（HTTP 401）—— 凭据缺失或已过期，请重新登录：astra-cli login / admin-cli login"
	case http.StatusForbidden:
		return msg + "（HTTP 403）—— 当前身份没有这个操作的权限"
	case http.StatusNotFound:
		return msg + "（HTTP 404）—— 对象不存在，或不属于当前账号"
	case http.StatusConflict:
		return msg + "（HTTP 409）—— 状态冲突，先刷新再看"
	case http.StatusTooManyRequests:
		return msg + "（HTTP 429）—— 触发限流或配额"
	case http.StatusNotImplemented:
		return msg + "（HTTP 501）—— 服务端未启用该能力"
	case http.StatusServiceUnavailable:
		return msg + "（HTTP 503）—— 依赖未就绪或能力未启用"
	default:
		return fmt.Sprintf("%s（HTTP %d）", msg, e.Status)
	}
}

// IsStatus 判断 err 是否为某个状态码的 APIError。CLI 用它决定退出码/提示。
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// IsUnauthorized 常见判断的语法糖。
func IsUnauthorized(err error) bool { return IsStatus(err, http.StatusUnauthorized) }

// ── 底层请求 ─────────────────────────────────────────────────

func (c *Client) endpoint(path string, q url.Values) string {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func (c *Client) newRequest(ctx context.Context, method, path string, q url.Values, body any) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, errors.New("未配置服务端地址：用 --server 或先 astra-cli login --server <URL>")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码请求体失败: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, q), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// decodeError 把非 2xx 响应压成 *APIError。
// 服务端所有错误都是 {"error": "..."}，但 5xx 可能来自反向代理（HTML），
// 所以解析失败时退回原始文本前 200 字节，至少能看出是哪一层挂的。
func decodeError(resp *http.Response, method, path string) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	ae := &APIError{Status: resp.StatusCode, Method: method, Path: path}
	var withErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &withErr) == nil && withErr.Error != "" {
		ae.Message = withErr.Error
		return ae
	}
	txt := strings.TrimSpace(string(raw))
	if len(txt) > 200 {
		txt = txt[:200] + "…"
	}
	ae.Message = txt
	return ae
}

// do 发一次带 JSON 响应的请求。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	req, err := c.newRequest(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	c.logfReq("→ %s %s", method, req.URL.Path)
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("请求被取消: %w", ctx.Err())
		}
		return fmt.Errorf("连不上 %s: %w", c.BaseURL, err)
	}
	c.logfReq("← %d %s (%s)", resp.StatusCode, req.URL.Path, time.Since(start).Round(time.Millisecond))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp, method, path)
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// 线上最常见的「莫名其妙」：路由不存在时，服务端的 SPA 兜底会返回 200 + index.html。
	// 直接 JSON 解码只会报 invalid character '<'，看不出根因。这里提前拦下来，
	// 把「服务端版本旧 / 路由没注册」这句话说出来。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		snippet := strings.Join(strings.Fields(string(raw)), " ")
		if len(snippet) > 100 {
			snippet = snippet[:100] + "…"
		}
		return fmt.Errorf("%s 返回的不是 JSON（Content-Type: %s）：%s。通常是服务端版本较旧、该路由未注册（被前端首页兜底了）", path, ct, snippet)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // 空响应体（例如 204 风格的 200），不算错
		}
		return fmt.Errorf("解析 %s 响应失败: %w", path, err)
	}
	return nil
}

func qs(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	return v
}
