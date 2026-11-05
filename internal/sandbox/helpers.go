package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultAgentCommand 推断本机该怎么起 agent。
//
// 查找顺序（前面的优先）：
//
//	ASTRA_AGENT 环境变量 → PATH 里的 dsh → ~/.astra/agent-dsh → 仓库 dsh-trim
//
// 放在 sandbox 包而不是 CLI，是因为「这台机器上 agent 在哪」是 provider 的本地知识：
// CLI 和 local provider 各写一份必然漂移，而漂移的后果是「doctor 说找到了、
// make dev 却用错命令」，排查起来很费时间。
func DefaultAgentCommand() []string {
	if v := os.Getenv("ASTRA_AGENT"); v != "" {
		return strings.Fields(v)
	}
	if _, err := exec.LookPath("dsh"); err == nil {
		return []string{"dsh", "--profile", "acp"}
	}

	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".astra", "agent-dsh", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}
	candidates = append(candidates,
		"dsh-trim/node_modules/@deepseek-ai/dsh/lib/bin.js",
		"../dsh-trim/node_modules/@deepseek-ai/dsh/lib/bin.js",
	)
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err == nil {
			return []string{"node", abs, "--profile", "acp"}
		}
	}
	return []string{"dsh", "--profile", "acp"}
}

// portFree 判断本机端口当前能否被绑定。
// 用于「在原端口重启」前的可用性检查——端口可能已被别的进程抢走。
func portFree(port int) bool {
	if port <= 0 {
		return false
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// reqToken 取请求里的 token，空则生成。各 provider 共用，避免三处各写一份随机逻辑。
func reqToken(req CreateRequest) string {
	if req.Token != "" {
		return req.Token
	}
	return randomToken()
}

// pickFreePort 从 start 起找一个可绑定的本机端口。
//
// 存在理论上的 TOCTOU：探测完到真正 bind 之间有窗口。本地开发场景接受这个风险；
// 生产 provider（cube）由平台分配端口，不走这里。
func pickFreePort(start int) (int, error) {
	for p := start; p < start+2000; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("sandbox: %d-%d 没有空闲端口", start, start+2000)
}

// waitHTTP 轮询 /health 直到 200 或超时。
// 用 /health 而不是「端口能连上」：端口能连只说明 socket 监听了，
// 而我们要等的是 agent 真正握手可用。
func waitHTTP(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health 返回 %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("超时 %s", timeout)
	}
	return lastErr
}

// ResolveProxyBin 定位 acp-proxy 二进制。
//
// 顺序：显式指定 → 环境变量 → PATH → 与当前可执行文件同目录 → 当前目录下的 bin/。
// 「同目录」这一档是必需的：`make build` 出来的二进制都在 bin/ 里，
// 用 ./bin/astra-server 启动时，acp-proxy 就在旁边，不该要求用户再设环境变量。
func ResolveProxyBin(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("ASTRA_PROXY_BIN"); env != "" {
		return env
	}
	if p, err := exec.LookPath("acp-proxy"); err == nil {
		return p
	}
	if p, ok := SiblingBinary("acp-proxy"); ok {
		return p
	}
	for _, c := range []string{"bin/acp-proxy", "./bin/acp-proxy"} {
		if abs, err := filepath.Abs(c); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	return ""
}

// SiblingBinary 在「当前可执行文件所在目录」找同名二进制。
// 返回绝对路径；找不到返回 false。
func SiblingBinary(name string) (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	// 解析软链：/tmp/x -> ~/bin/x 时，二进制实际在 ~/bin 而不是 /tmp。
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p, true
	}
	return "", false
}
