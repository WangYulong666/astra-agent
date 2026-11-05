package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ── 身份 ─────────────────────────────────────────────────────

// Login 用租户 slug + 邮箱 + 密码换 token。
// 服务端 login 需要 tenant slug（默认 default），所以这三个参数都必填。
func (c *Client) Login(ctx context.Context, tenant, email, password string) (LoginResult, error) {
	var out LoginResult
	body := map[string]string{"tenant": tenant, "email": email, "password": password}
	err := c.do(ctx, http.MethodPost, "/v1/auth/login", nil, body, &out)
	return out, err
}

// Meta 取运行信息（provider / agent / 当前身份）。whoami 用它。
func (c *Client) Meta(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	err := c.do(ctx, http.MethodGet, "/v1/meta", nil, nil, &out)
	return out, err
}

// ── 会话 ─────────────────────────────────────────────────────

// ListSessions 列出当前身份可见的会话。
func (c *Client) ListSessions(ctx context.Context) ([]Session, error) {
	var out []Session
	err := c.do(ctx, http.MethodGet, "/v1/sessions", nil, nil, &out)
	return out, err
}

// GetSession 取单个会话。
func (c *Client) GetSession(ctx context.Context, id string) (Session, error) {
	var out Session
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// CreateSession 建会话。
//
// 注意：POST /v1/sessions 只接受 cwd，标题要另走 PATCH——所以 title 非空时
// 这里发两次请求。不把 title 塞进 POST 请求体（服务端会静默忽略），否则
// 用户会看到「标题没生效」这种最难查的假成功。
func (c *Client) CreateSession(ctx context.Context, title string) (Session, error) {
	var out Session
	if err := c.do(ctx, http.MethodPost, "/v1/sessions", nil, map[string]string{}, &out); err != nil {
		return out, err
	}
	if strings.TrimSpace(title) == "" {
		return out, nil
	}
	upd, err := c.RenameSession(ctx, out.ID, title)
	if err != nil {
		// 会话已经建好了，标题失败不该让整个命令失败——user 拿到 id 才最重要。
		return out, nil
	}
	return upd, nil
}

// RenameSession 改标题。
func (c *Client) RenameSession(ctx context.Context, id, title string) (Session, error) {
	var out Session
	err := c.do(ctx, http.MethodPatch, "/v1/sessions/"+url.PathEscape(id), nil,
		map[string]string{"title": title}, &out)
	return out, err
}

// DeleteSession 删会话（同时销毁沙箱）。
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(id), nil, nil, nil)
}

// ── run ─────────────────────────────────────────────────────

// CreateRun 提交一条指令。
func (c *Client) CreateRun(ctx context.Context, sessionID, prompt string) (Run, error) {
	var out Run
	err := c.do(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/runs", nil,
		map[string]string{"input": prompt}, &out)
	return out, err
}

// GetRun 查 run 当前状态。
func (c *Client) GetRun(ctx context.Context, id string) (Run, error) {
	var out Run
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}

// CancelRun 取消 run。
func (c *Client) CancelRun(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/cancel", nil, nil, nil)
}

// ListRuns 列会话下的 run。
func (c *Client) ListRuns(ctx context.Context, sessionID string) ([]Run, error) {
	var out []Run
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/runs", nil, nil, &out)
	return out, err
}

// History 一次取回已有事件（SSE 的 JSON 版），用于「不跟随、只要结果」的场景。
func (c *Client) History(ctx context.Context, runID string, after int64) ([]Event, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after_seq", strconv.FormatInt(after, 10))
	}
	var raw []struct {
		Seq       int64           `json:"seq"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
		CreatedAt json.RawMessage `json:"createdAt"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/history", q, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(raw))
	for _, r := range raw {
		out = append(out, Event{Seq: r.Seq, Type: r.Type, Data: r.Payload})
	}
	return out, nil
}

// ── 审批 ─────────────────────────────────────────────────────

// ListApprovals 列 run 下的审批请求。
func (c *Client) ListApprovals(ctx context.Context, runID string) ([]Approval, error) {
	var out []Approval
	err := c.do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/approvals", nil, nil, &out)
	return out, err
}

// ResolveApproval 提交人工审批结论。decision 只接受 allow / deny。
func (c *Client) ResolveApproval(ctx context.Context, runID, toolCallID, decision string) error {
	switch decision {
	case "allow", "deny":
	default:
		return fmt.Errorf("decision 只能是 allow 或 deny，收到 %q", decision)
	}
	path := "/v1/runs/" + url.PathEscape(runID) + "/approvals/" + url.PathEscape(toolCallID)
	return c.do(ctx, http.MethodPost, path, nil, map[string]string{"decision": decision}, nil)
}

// ── 文件 ─────────────────────────────────────────────────────

// ListFiles 列会话工作区目录。path 为空表示根目录。
func (c *Client) ListFiles(ctx context.Context, sessionID, path string) (FileList, error) {
	var out FileList
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/files",
		qs("path", path), nil, &out)
	return out, err
}

// ReadFile 读文件预览。二进制文件服务端不返回内容（Binary=true），
// 要落盘请用 DownloadFile——那个端点返回原始字节。
func (c *Client) ReadFile(ctx context.Context, sessionID, path string) (FileContent, error) {
	var out FileContent
	err := c.do(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/file",
		qs("path", path), nil, &out)
	return out, err
}

// DownloadFile 把文件原始字节写进 w，返回字节数。
//
// 不走 do()：那条路径假设响应是 JSON。这里必须原样流式拷贝，
// 否则大文件会先全读进内存，二进制内容也会被 JSON 解码毁掉。
func (c *Client) DownloadFile(ctx context.Context, sessionID, path string, w io.Writer) (int64, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID)+"/download",
		qs("path", path), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	c.logfReq("→ GET %s", req.URL.RequestURI())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("下载被取消: %w", ctx.Err())
		}
		return 0, fmt.Errorf("连不上 %s: %w", c.BaseURL, err)
	}
	c.logfReq("← %d %s", resp.StatusCode, req.URL.Path)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, decodeError(resp, http.MethodGet, req.URL.Path)
	}
	// 同上：路由缺失时 SPA 兜底会回 index.html。直接落盘等于存了一个假文件。
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		resp.Body.Close()
		return 0, fmt.Errorf("%s 返回了 HTML 而不是文件内容——通常是服务端版本较旧、下载路由未注册", req.URL.Path)
	}
	defer resp.Body.Close()
	return io.Copy(w, resp.Body)
}

// ── 事件流（SSE）─────────────────────────────────────────────

// streamHTTP 返回一个不设总超时的 client。
//
// 为什么必须分开：一次长命令可能几分钟没有输出，30s 的总超时会在
// 「一切正常、只是慢」的时候把流掐断；而 SSE 的终止条件只有
// 「收到终态事件」和「ctx 被取消」两种。
func (c *Client) streamHTTP() *http.Client {
	return &http.Client{Transport: c.HTTP.Transport}
}

// RunEvents 订阅 run 的事件流。after>0 时从该 seq 之后续传。
//
// 返回的 channel 在以下情况关闭：收到终态事件、服务端关流、ctx 取消、读流失败。
// 读流失败会先投递一条 Type=StreamClientError 的事件再关闭，调用方据此区分
// 「正常结束」和「连接断了」。
func (c *Client) RunEvents(ctx context.Context, runID string, after int64) (<-chan Event, error) {
	q := url.Values{}
	if after > 0 {
		q.Set("after_seq", strconv.FormatInt(after, 10))
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(runID)+"/events", q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.logfReq("→ GET %s (SSE)", req.URL.RequestURI())

	resp, err := c.streamHTTP().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("订阅被取消: %w", ctx.Err())
		}
		return nil, fmt.Errorf("订阅事件流失败: %w", err)
	}
	c.logfReq("← %d %s (SSE)", resp.StatusCode, req.URL.Path)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeError(resp, http.MethodGet, req.URL.Path)
	}

	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		readSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// readSSE 解析 text/event-stream。
//
// 只实现协议里服务端真的会发的部分（见 api.go 的 writeEvent / events）：
//
//	retry: 1000\n\n                     ← 握手后第一行
//	id: 12\nevent: message_delta\ndata: {"text":"hi"}\n\n
//	: ping\n\n                          ← 心跳注释帧
//	event: overflow\ndata: {}\n\n       ← 缓冲满，客户端应重连续传
//
// 多行 data 按规范用 \n 拼接：当前服务端的 payload 是单行 JSON，
// 但一旦有人往 data 里塞换行，不拼接就会解析出半个 JSON。
func readSSE(ctx context.Context, body io.Reader, ch chan<- Event) {
	sc := bufio.NewScanner(body)
	// 事件里可能带整段文件内容（tool_call 的 rawOutput），默认 64KB 行上限会截断。
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)

	var (
		seq  int64
		typ  string
		data []string
	)
	dispatch := func() bool {
		if typ == "" && len(data) == 0 {
			return true
		}
		ev := Event{Seq: seq, Type: typ}
		if len(data) > 0 {
			payload := strings.Join(data, "\n")
			ev.Data = json.RawMessage(payload)
			if !json.Valid(ev.Data) {
				// 非法 JSON 不该让整个流崩掉：包成一条 error 事件，让上层能看见原文。
				b, _ := json.Marshal(map[string]string{"error": "事件数据不是合法 JSON", "raw": payload})
				ev.Data = b
				ev.Type = EventError
			}
		}
		select {
		case ch <- ev:
		case <-ctx.Done():
			return false
		}
		seq, typ, data = 0, "", nil
		return true
	}

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if !dispatch() {
				return
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释 / 心跳
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				seq = n
			}
		case "event":
			typ = strings.TrimSpace(value)
		case "data":
			data = append(data, value)
		}
		// retry / 未知字段忽略。
	}
	dispatch() // 末尾没有空行时，把最后一条也送出去

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		select {
		case ch <- Event{Type: StreamClientError, Data: payload}:
		case <-ctx.Done():
		}
	}
}
