package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	if _, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		t.Error("Close 后仍能连上代理端口")
	}
	// 端口必须真的释放，而不只是服务不 accept。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Close 后端口未释放: %v", err)
	}
	_ = ln.Close()

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
