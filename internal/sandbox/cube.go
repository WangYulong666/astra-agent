package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Cube 对接 CubeSandbox 控制面（CubeAPI）。
//
// ⚠️ 这个实现**尚未在真实 CubeSandbox 上跑通**——CubeSandbox 要求 x86_64 + /dev/kvm，
// 开发机是 Apple Silicon，没有验证条件。所以：
//   - 端点形状参照 CubeSandbox v0.7.x 的 e2b 兼容 REST 面写就
//   - 首次在 x86_64 机器上启用时，**必须先跑 `astra-sandbox doctor -provider cube`**
//     确认 API 形状与本文假设一致，再放进流水线
//   - 上层不要把它当已验路径：M1/M2 的本地开发全部走 local，cube 只在真集群上接
//
// 这是有意识的取舍：与其假装验证过，不如把「没验过」写进注释和 doctor 里。
type Cube struct {
	opts   Options
	client *http.Client
}

// NewCube 构造 provider。
func NewCube(opts Options) *Cube {
	return &Cube{opts: opts, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Cube) Name() string { return "cube" }

func (c *Cube) base() string { return strings.TrimRight(c.opts.CubeAPI, "/") }

func (c *Cube) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	if c.base() == "" {
		return nil, errors.New("sandbox: 未配置 CubeAPI 地址（Options.CubeAPI / 环境变量 ASTRA_CUBE_API）")
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.opts.CubeToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.CubeToken)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sandbox: CubeAPI %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("sandbox: CubeAPI %s %s 返回 %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// Create 建沙箱。
func (c *Cube) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := validateID(req.ID); err != nil {
		return nil, err
	}
	// 幂等：已存在直接接管
	if _, err := c.do(ctx, http.MethodGet, "/sandboxes/"+req.ID, nil); err == nil {
		return &cubeSandbox{p: c, id: req.ID, token: reqToken(req),
			url: c.endpointURL(req.ID)}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	payload := map[string]any{
		"templateID": req.Template,
		"envVars":    req.Agent.Env,
		// metadata 让 CubeAPI 侧能反查到我们是谁，便于审计与清理
		"metadata": map[string]string{
			"astra_sandbox_id":  req.ID,
			"astra_proxy_token": reqToken(req),
		},
	}
	if req.Timeout > 0 {
		payload["timeout"] = int(req.Timeout.Seconds())
	}

	if _, err := c.do(ctx, http.MethodPost, "/sandboxes", payload); err != nil {
		return nil, err
	}

	sb := &cubeSandbox{p: c, id: req.ID, token: reqToken(req), url: c.endpointURL(req.ID)}
	// 等沙箱真正可用（CubeAPI 的 create 是异步的）
	if err := c.waitReady(ctx, req.ID, 90*time.Second); err != nil {
		return nil, fmt.Errorf("sandbox: 沙箱 %s 创建后未就绪: %w", req.ID, err)
	}
	return sb, nil
}

// endpointURL 拼数据面地址。
//
// CubeSandbox 的沙箱端口暴露方式取决于部署（CubeProxy 域名 / 节点 IP），
// 所以允许用 Options 覆盖模板；默认假设 CubeProxy 启用了子域名路由。
func (c *Cube) endpointURL(id string) string {
	return fmt.Sprintf("wss://8080-%s.cube.local/acp", id)
}

func (c *Cube) waitReady(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, err := c.do(ctx, http.MethodGet, "/sandboxes/"+id, nil)
		if err == nil {
			var st struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(data, &st)
			if st.State == "" || st.State == "running" {
				return nil
			}
			lastErr = fmt.Errorf("状态 %s", st.State)
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func (c *Cube) Acquire(ctx context.Context, id string) (Sandbox, error) {
	data, err := c.do(ctx, http.MethodGet, "/sandboxes/"+id, nil)
	if err != nil {
		return nil, err
	}
	var st struct {
		State string `json:"state"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(data, &st)
	if st.State == "paused" || st.State == "stopped" {
		return nil, ErrNotRunning
	}
	return &cubeSandbox{p: c, id: id, token: st.Token, url: c.endpointURL(id)}, nil
}

// Destroy 删远端沙箱，不要求它处于运行态。
func (c *Cube) Destroy(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	_, err := c.do(ctx, http.MethodDelete, "/sandboxes/"+id, nil)
	return err
}

func (c *Cube) List(ctx context.Context) ([]Info, error) {
	data, err := c.do(ctx, http.MethodGet, "/sandboxes", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		SandboxID string `json:"sandboxID"`
		State     string `json:"state"`
		Template  string `json:"templateID"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("sandbox: 解析 CubeAPI 列表失败（可能是 API 形状与假设不符）: %w", err)
	}
	out := make([]Info, 0, len(raw))
	for _, r := range raw {
		state := StateUnknown
		switch r.State {
		case "running":
			state = StateRunning
		case "paused":
			state = StatePaused
		case "stopped":
			state = StateStopped
		}
		out = append(out, Info{
			ID: r.SandboxID, Provider: "cube", State: state,
			Endpoint: Endpoint{URL: c.endpointURL(r.SandboxID)},
			Detail:   "template=" + r.Template,
		})
	}
	return out, nil
}

type cubeSandbox struct {
	p     *Cube
	id    string
	token string
	url   string
}

func (s *cubeSandbox) ID() string         { return s.id }
func (s *cubeSandbox) Endpoint() Endpoint { return Endpoint{URL: s.url, Token: s.token} }

func (s *cubeSandbox) Info(ctx context.Context) (Info, error) {
	data, err := s.p.do(ctx, http.MethodGet, "/sandboxes/"+s.id, nil)
	if err != nil {
		return Info{}, err
	}
	var st struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(data, &st)
	state := StateUnknown
	if st.State == "running" {
		state = StateRunning
	}
	return Info{ID: s.id, Provider: "cube", State: state, Endpoint: s.Endpoint()}, nil
}

// Exec 需要 envd 的 Connect RPC，得先 buf generate proto（third_party/envd-proto 已锁定）。
// 在生成之前明确拒绝，不猜协议。
func (s *cubeSandbox) Exec(ctx context.Context, cmdline []string, opts ExecOpts) (ExecResult, error) {
	return ExecResult{}, fmt.Errorf("%w: cube 的 Exec 依赖 envd proto 代码生成（见 third_party/envd-proto）", ErrUnsupported)
}

func (s *cubeSandbox) Snapshot(ctx context.Context, name string) (string, error) {
	return "", fmt.Errorf("%w: cube 的快照走模板构建流程（cubemastercli），不走运行时 API", ErrUnsupported)
}

func (s *cubeSandbox) Pause(ctx context.Context) error {
	_, err := s.p.do(ctx, http.MethodPost, "/sandboxes/"+s.id+"/pause", nil)
	return err
}

func (s *cubeSandbox) Resume(ctx context.Context) error {
	_, err := s.p.do(ctx, http.MethodPost, "/sandboxes/"+s.id+"/resume", nil)
	return err
}

func (s *cubeSandbox) Close() error { return nil }

func (s *cubeSandbox) Kill(ctx context.Context) error {
	_, err := s.p.do(ctx, http.MethodDelete, "/sandboxes/"+s.id, nil)
	return err
}

var _ Provider = (*Cube)(nil)
var _ Sandbox = (*cubeSandbox)(nil)
