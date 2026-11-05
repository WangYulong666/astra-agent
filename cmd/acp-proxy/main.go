// Command acp-proxy 在沙箱内把 agent 的 stdio 桥接成 WSS 端点。
//
// 为什么需要它（见 ADR-0002）：
//
//	控制面直接持有 agent 的 stdio pipe 时，worker 一重启 pipe 就断，agent 变成孤儿进程；
//	滚动更新则会把正在跑的长任务直接杀掉。把 stdio 桥成网络端点后，worker 变成无状态的，
//	断线只是「暂时没人订阅」，agent 继续活着，重连后用 session/resume 恢复执行态。
//
// 三条设计约束：
//
//  1. 同一时刻只服务一条 WSS 连接。ACP 是独占会话，两路 client 接入会让 JSON-RPC 的 id
//     命名空间冲突，且 request_permission 会被任意一方抢答。
//  2. **不缓存帧**。没有客户端时 stdout 的内容直接丢弃（并计数）。历史由平台侧的事件表
//     重建，proxy 不持有「第二份真相」。
//  3. stderr 必须一直排空。agent 把日志写 stderr，管道写满（约 64KB）就会阻塞 agent，
//     表现为「连上了但 agent 永远不说话」。
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/WangYulong666/astra-agent/internal/acp"
)

// 单帧上限。ACP 的 session/update 里可能带大段工具输出，8MB 是安全余量。
const maxFrame = 8 << 20

func main() {
	var (
		listen   = flag.String("listen", ":8080", "监听地址")
		agentCmd = flag.String("agent", "dsh --profile acp", "agent 启动命令（按空格切分，不支持引号）")
		cwd      = flag.String("cwd", "/workspace", "agent 工作目录")
		token    = flag.String("token", os.Getenv("ASTRA_PROXY_TOKEN"),
			"连接鉴权 token（默认读环境变量 ASTRA_PROXY_TOKEN）；空表示不校验，仅限本地开发")
		logLevel = flag.String("log", "info", "日志级别：debug/info/warn/error")
	)
	flag.Parse()

	setupLogger(*logLevel)

	parts := strings.Fields(*agentCmd)
	if len(parts) == 0 {
		slog.Error("agent 命令为空")
		os.Exit(1)
	}

	// agent 起来之前先保证 cwd 存在，否则 dsh 启动即失败
	if *cwd != "" {
		if err := os.MkdirAll(*cwd, 0o755); err != nil {
			slog.Warn("创建 cwd 失败", "cwd", *cwd, "err", err)
		}
	}

	agent, err := acp.StartStdio(parts[0], parts[1:], acp.WithDir(*cwd), acp.WithStderr(os.Stderr))
	if err != nil {
		slog.Error("启动 agent 失败", "cmd", *agentCmd, "err", err)
		os.Exit(1)
	}
	slog.Info("agent 已启动", "cmd", *agentCmd, "pid", agent.PID(), "cwd", *cwd)

	b := &bridge{agent: agent}
	b.alive.Store(true)

	// stdout → WSS 的常驻泵。它比任何连接都活得久，连接只是「订阅者」。
	go b.pumpStdout(agent)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           newRouter(b, *token),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("acp-proxy 监听", "addr", *listen, "auth", *token != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP 服务异常", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("开始关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = agent.Close()
	slog.Info("已退出")
}

// ── 桥接 ──────────────────────────────────────────────────────

type bridge struct {
	agent *acp.StdioConn

	mu   sync.Mutex
	conn *websocket.Conn // nil 表示当前没有客户端

	alive   atomic.Bool
	dropped atomic.Int64
}

// attach 抢占唯一连接槽位。已有连接时返回 false。
func (b *bridge) attach(c *websocket.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return false
	}
	b.conn = c
	return true
}

func (b *bridge) detach(c *websocket.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == c {
		b.conn = nil
	}
}

func (b *bridge) attached() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn != nil
}

// pumpStdout 持续读 agent 的 stdout，按行分帧推给当前客户端。
//
// 按 '\n' 切分是安全的：JSON 字符串内部的换行会被转义成 \n，不会出现裸换行。
func (b *bridge) pumpStdout(r io.Reader) {
	defer b.alive.Store(false)

	sc := bufio.NewScanner(r)
	// 初始 64KB，按需增长到 maxFrame（Scanner 会自己扩，不需要一上来就分配 maxFrame）
	sc.Buffer(make([]byte, 0, 64<<10), maxFrame)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		b.send(line)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("读 agent stdout 结束", "err", err)
	}
	slog.Info("agent stdout 已关闭，agent 退出")
}

// send 把一帧推给当前客户端；没有客户端就丢弃并计数。
func (b *bridge) send(msg []byte) {
	b.mu.Lock()
	c := b.conn
	b.mu.Unlock()

	if c == nil {
		b.dropped.Add(1)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
		slog.Debug("写 WSS 失败，断开该连接", "err", err)
		b.detach(c)
		_ = c.CloseNow()
	}
}

// readWS 把客户端消息写进 agent 的 stdin。
// 约定：每帧是一条完整 JSON，不含结尾换行，这里补 '\n' 满足 NDJSON。
func (b *bridge) readWS(ctx context.Context, c *websocket.Conn) error {
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}
		buf := make([]byte, len(data)+1)
		copy(buf, data)
		buf[len(data)] = '\n'
		if _, err := b.agent.Write(buf); err != nil {
			return fmt.Errorf("写 agent stdin 失败: %w", err)
		}
	}
}

// ── HTTP ──────────────────────────────────────────────────────

func newRouter(b *bridge, token string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if !b.alive.Load() {
			status = "agent-dead"
		}
		w.Header().Set("Content-Type", "application/json")
		if status != "ok" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":        status,
			"attached":      b.attached(),
			"droppedFrames": b.dropped.Load(),
		})
	})

	mux.HandleFunc("GET /acp", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !b.alive.Load() {
			http.Error(w, "agent is not running", http.StatusServiceUnavailable)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// 沙箱内部端点，TLS 由 CubeNet / ingress 终结，这里不做 Origin 校验
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			slog.Debug("WSS 握手失败", "err", err)
			return
		}
		c.SetReadLimit(maxFrame)

		if !b.attach(c) {
			// 明确告诉新连接「已有人在用」，而不是静默排队——ACP 不能两路并存
			_ = c.Close(websocket.StatusPolicyViolation, "another client is attached")
			return
		}
		defer func() {
			b.detach(c)
			_ = c.CloseNow()
		}()

		slog.Info("客户端已接入", "remote", r.RemoteAddr)
		err = b.readWS(r.Context(), c)
		slog.Info("客户端断开", "remote", r.RemoteAddr, "err", err)
	})

	return mux
}

func authorized(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got := strings.TrimPrefix(h, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	got := r.Header.Get("X-Astra-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func setupLogger(level string) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})))
}
