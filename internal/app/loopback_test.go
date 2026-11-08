package app

import (
	"net"
	"strconv"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/runner"
)

func TestNormalizeLoopbackThreeInputs(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		// 回环地址正常端口。
		{"127.0.0.1:port", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7788}, "http://127.0.0.1:7788"},
		// IPv4 通配地址 0.0.0.0:port（ln.Addr().String() 可能是这个）。
		{"0.0.0.0:port", &net.TCPAddr{IP: net.IPv4zero, Port: 8080}, "http://127.0.0.1:8080"},
		// IPv6 通配地址 [::]:port（监听 :0 时 Go 常返回这个）。
		{"[::]:port", &net.TCPAddr{IP: net.IPv6zero, Port: 9000}, "http://127.0.0.1:9000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeLoopback(c.addr, "FALLBACK"); got != c.want {
				t.Fatalf("normalizeLoopback(%v) = %q, want %q", c.addr, got, c.want)
			}
		})
	}
}

// fakeAddr 没有 host:port，用来触发 fallback 分支。
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "not-a-host-port" }

func TestNormalizeLoopbackFallback(t *testing.T) {
	if got := normalizeLoopback(nil, "http://fallback"); got != "http://fallback" {
		t.Fatalf("nil addr 应退回 fallback, got %q", got)
	}
	if got := normalizeLoopback(fakeAddr{}, "http://fallback"); got != "http://fallback" {
		t.Fatalf("取不到端口应退回 fallback, got %q", got)
	}
	// 端口 0（未分配的 :0）不该被当成真实端口。
	if got := normalizeLoopback(&net.TCPAddr{IP: net.IPv4zero, Port: 0}, "http://fallback"); got != "http://fallback" {
		t.Fatalf("端口 0 应退回 fallback, got %q", got)
	}
}

// Start 之前 hints 的 url 必须为空（控制面还没监听，不该注入连不上的
// ASTRA_SERVER）；Start 之后由 normalizeLoopback 回填成回环可达地址。
// cliBin 在 New 里 Set 一次，SetURL 只改 url，不能把它清掉。
func TestHintsURLNotReadyBeforeStart(t *testing.T) {
	h := runner.NewRuntimeHints()
	cli := "/data/bin/astra-cli"
	h.Set(nil, "", cli) // 模拟 app.New
	_, url, gotCLI := h.Snapshot()
	if url != "" {
		t.Fatalf("Start 前 url 应为空, got %q", url)
	}
	if gotCLI != cli {
		t.Fatalf("Start 前 cliBin 应已就位, got %q", gotCLI)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	// 模拟 Start：用通配地址监听，确认也被规整成 127.0.0.1。
	h.SetURL(normalizeLoopback(&net.TCPAddr{IP: net.IPv6zero, Port: ln.Addr().(*net.TCPAddr).Port},
		"http://127.0.0.1:0"))
	_, url, gotCLI = h.Snapshot()
	if url != "http://127.0.0.1:"+port {
		t.Fatalf("Start 后 url = %q, want http://127.0.0.1:%s", url, port)
	}
	if gotCLI != cli {
		t.Fatalf("SetURL 不该清掉 cliBin, got %q", gotCLI)
	}
}
