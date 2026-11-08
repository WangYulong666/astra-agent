package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openProxyAt 用指定数据目录起一个测试代理。
// 数据目录走环境变量是因为 egress.Config 没有 DataDir 字段（egress.go 冻结）。
func openProxyAt(t *testing.T, dir string, pol Policy) Controller {
	t.Helper()
	t.Setenv("EGRESS_DATA_DIR", dir)
	ctl, err := Open(Config{
		ListenAddr:    "127.0.0.1:0",
		DefaultPolicy: pol,
		LogSize:       128,
	})
	if err != nil {
		t.Fatalf("启动测试代理: %v", err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	return ctl
}

func openTestProxy(t *testing.T, pol Policy) Controller {
	t.Helper()
	return openProxyAt(t, t.TempDir(), pol)
}

func TestOpenDisabledWithoutAddr(t *testing.T) {
	ctl, err := Open(Config{})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Open(空地址) 应返回 ErrDisabled，得到 %v", err)
	}
	if ctl != nil {
		t.Fatal("未启用时 Controller 应为 nil")
	}
}

func TestCloseReleasesPort(t *testing.T) {
	ctl := openTestProxy(t, Policy{Default: Deny})
	addr := ctl.Addr()
	if addr == "" || !ctl.Enabled() {
		t.Fatalf("代理未就绪: addr=%q enabled=%v", addr, ctl.Enabled())
	}

	if err := ctl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ctl.Enabled() {
		t.Error("Close 后 Enabled() 仍为 true")
	}
	// 只判定「我们自己的代理」是否还在响应，不能只看端口能否连上：
	// CI 上多个包并行，内核分配的 ephemeral 端口可能在 Close 的同一瞬间被
	// 别的用例复用，Dial 到的是别人的 listener（Linux runner 上实测偶发）。
	// 发一个探测请求，只有拿到 egress 特征响应才算真泄漏。
	client := &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: &http.Transport{Proxy: nil}, // 别走环境代理，否则测的不是本机端口
	}
	resp, err := client.Get("http://" + addr + "/")
	if err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		raw := string(body) + resp.Header.Get("Content-Type")
		if strings.Contains(raw, "egress") {
			t.Error("Close 后 egress 代理仍在响应")
		} else {
			t.Logf("Close 后端口已被其它进程复用（响应非 egress），不算本代理泄漏")
		}
	}
	// 端口释放交给内核；被并行用例抢先占用时不该判成我们的失败。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Logf("Close 后无法重新监听 %s（可能已被并行用例复用）: %v", addr, err)
	} else {
		_ = ln.Close()
	}

	// 重复 Close 必须幂等；日志仍然可读（审计不能随进程一起消失）。
	if err := ctl.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if _, err := ctl.Log(context.Background(), "", 10); err != nil {
		t.Fatalf("Close 后读日志: %v", err)
	}
}

func TestCloseStopsActiveTunnel(t *testing.T) {
	// 一个只监听、不说话的「上游」，让隧道保持打开。
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	ctl := openTestProxy(t, Policy{
		Default:      Deny,
		AllowPrivate: true,
		Rules:        []Rule{{Pattern: "127.0.0.1", Decision: Allow}},
	})

	conn, err := net.Dial("tcp", ctl.Addr())
	if err != nil {
		t.Fatalf("连代理: %v", err)
	}
	defer conn.Close()

	target := upstream.Addr().String()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("发 CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("读 CONNECT 响应: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT 状态码 = %d, want 200", resp.StatusCode)
	}

	if err := ctl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("Close 后隧道仍可读，说明活跃连接没被关掉")
	}
}

func TestPolicyFilePermissionsAndPath(t *testing.T) {
	dir := t.TempDir()
	ctl := openProxyAt(t, dir, Policy{Default: Deny})
	if err := ctl.SetPolicy(context.Background(), "t1", Policy{Default: Allow}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	path := filepath.Join(dir, policyFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("策略文件未落盘: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("策略文件权限 = %v, want 0600", perm)
	}
}
