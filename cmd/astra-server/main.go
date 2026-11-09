// astra-server 是控制面的命令行入口。
//
// 装配逻辑全部在 internal/app：这个文件只做三件事——解析 flag、接退出信号、
// 把配置交给 app.New。桌面端（cmd/astra-desktop）复用同一个包，两边的
// 行为差异只体现在「配置从哪来」和「窗口/日志长什么样」。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/WangYulong666/astra-agent/internal/app"
)

func main() {
	// .env 必须在 flag.Parse 之前加载：配置项里有从环境变量读的
	// （MILVUS_ADDR / LANGFUSE_* 等可选能力），解析完再加载就晚了。
	// 已 export 的变量优先，方便临时覆盖。
	if n := app.LoadDotEnv(".env", nil); n > 0 {
		log.Printf("已从 .env 加载 %d 个变量", n)
	}

	var (
		addr     = flag.String("addr", "127.0.0.1:7788", "监听地址")
		dataDir  = flag.String("data", "", "数据目录（默认 ~/.astra/data）")
		provider = flag.String("provider", "local", "沙箱 provider：local / applecontainer / cube")
		agentCmd = flag.String("agent", "shell", `agent：shell（本地 acp-shell-agent）/ dsh / 自定义命令`)
		policy   = flag.String("policy", "ask", "权限预设：ask（人工审批）/ workspace-write / allow-all / deny-all")
		proxyBin = flag.String("proxy", "", "acp-proxy 路径，空则自动查找（internal/app 目前取默认解析，此 flag 保留兼容）")
		cwd      = flag.String("cwd", "", "会话初始工作目录，空则用沙箱 workspace")
		quiet    = flag.Bool("q", false, "只输出错误日志")

		// ── 存储与治理 ──
		storeKind = flag.String("store", "sqlite", "存储后端：sqlite / postgres / file（file 为 M1 旧实现，不支持租户与计费）")
		dsn       = flag.String("dsn", "", "数据库 DSN；空则用 <data>/astra.db（sqlite）")
		jwtSecret = flag.String("jwt-secret", "", "JWT 签名密钥；空则随机生成（重启后 token 全失效）。生产必须显式指定")
		requireAu = flag.Bool("require-auth", true, "强制登录。设为 false 时带凭据才解析、不带也放行（灰度期用）")
		// migrateOnly 给 k8s 的迁移 Job 用：迁移完成后不进 HTTP 监听，Job 才能结束。
		migrateOnly   = flag.Bool("migrate-only", false, "只跑数据库迁移然后退出（供 k8s Job / CI 用）")
		bootstrapFlag = flag.Bool("bootstrap", true, "空系统时自动播种默认租户与管理员；桌面端/向导模式关掉")

		// ── LLM 网关 ──
		llmGateway = flag.Bool("llm-gateway", false, "启用 LLM 网关：真 key 只留在控制面，agent 用短期虚拟 key")
		llmUpstr   = flag.String("llm-upstream", "https://api.deepseek.com", "上游 base_url")
		llmKey     = flag.String("llm-key", "", "上游真 key；空则取 DEEPSEEK_API_KEY 环境变量")
		llmProv    = flag.String("llm-provider", "deepseek", "默认上游 provider 名")
		// llmPublicURL 是**agent 视角**可达的网关根地址，不是控制面自己监听的地址。
		llmPublicURL = flag.String("llm-public-url", "", "网关对 agent 可见的根地址（如 http://127.0.0.1:7788）；空则不做虚拟 key 注入")
		llmKeyTTL    = flag.String("llm-key-ttl", "", "虚拟 key 有效期（如 2h）；空则用默认 2h")

		// ── 沙箱生命周期治理 ──
		sbSweep  = flag.Duration("sandbox-sweep", app.DefaultSweep, "沙箱回收扫描周期；0 关闭后台回收")
		sbIdle   = flag.Duration("sandbox-idle", app.DefaultIdle, "会话空闲多久回收沙箱；0 不按空闲回收")
		sbMaxAge = flag.Duration("sandbox-max-age", app.DefaultMaxAge, "沙箱最长存活时间；0 不限")
		sbQuota  = flag.Int("sandbox-quota", 0, "每租户并发沙箱上限；0 不限")
	)
	var agentEnv envFlags
	flag.Var(&agentEnv, "agent-env", "传给 agent 的环境变量，KEY=VALUE（可重复）；只写 KEY 表示从本进程环境转发")
	flag.Parse()
	_ = *proxyBin // internal/app 内部走 SiblingBinary 默认解析；flag 保留向后兼容

	lg := log.New(os.Stderr, "astra ", log.LstdFlags|log.Lmicroseconds)
	if *quiet {
		// 必须是 io.Discard，不能是 nil：log.Logger 的 Write 会解引用它。
		lg.SetOutput(io.Discard)
	}

	dir := *dataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatal(lg, fmt.Errorf("找不到用户目录，请用 -data 指定: %w", err))
		}
		dir = filepath.Join(home, ".astra", "data")
	}

	// migrate-only 路径：用完整装配跑迁移然后退出。迁移在 openStore 内部完成。
	if *migrateOnly {
		cfg := app.Config{DataDir: dir, StoreKind: *storeKind, DSN: *dsn, Logger: lg}
		s, err := app.New(cfg)
		if err != nil {
			fatal(lg, err)
		}
		_ = s.Close()
		lg.Printf("迁移完成（store=%s），退出", *storeKind)
		return
	}

	cfg := app.Config{
		Addr: *addr, DataDir: dir, Logger: lg,
		Provider: *provider, Agent: *agentCmd,
		AgentEnv: agentEnv.map0(), Policy: *policy, Cwd: *cwd,
		StoreKind: *storeKind, DSN: *dsn, RequireAuth: *requireAu, Bootstrap: *bootstrapFlag,
		LLMGateway: *llmGateway, LLMUpstream: *llmUpstr, LLMKey: *llmKey,
		LLMProvider: *llmProv, LLMPublicURL: *llmPublicURL,
		Sweep: lifecycleDuration(*sbSweep), Idle: lifecycleDuration(*sbIdle), MaxAge: lifecycleDuration(*sbMaxAge), Quota: *sbQuota,
	}
	if *jwtSecret != "" {
		cfg.JWTSecret = []byte(*jwtSecret)
	}
	if *llmKeyTTL != "" {
		d, err := time.ParseDuration(*llmKeyTTL)
		if err != nil || d <= 0 {
			fatal(lg, fmt.Errorf("-llm-key-ttl 非法: %q", *llmKeyTTL))
		}
		cfg.LLMKeyTTL = d
	}

	s, err := app.New(cfg)
	if err != nil {
		fatal(lg, err)
	}
	defer s.Close()

	url, err := s.Start()
	if err != nil {
		fatal(lg, err)
	}
	fmt.Printf("→ 打开 http://%s\n", url)

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		lg.Printf("收到退出信号，正在停止…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		close(done)
	}()

	select {
	case err := <-s.Err():
		fatal(lg, err)
	case <-done:
	}
	lg.Printf("已退出")
}

// envFlags 收集可重复的 -agent-env。
type envFlags []string

func (e *envFlags) String() string { return strings.Join(*e, ",") }

func (e *envFlags) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("空的 -agent-env")
	}
	*e = append(*e, v)
	return nil
}

// map 展开成环境变量表。只写 KEY 的项从本进程环境取值，取不到就跳过
// （让 agent 自己报「缺凭据」，比在服务端静默传空串更好排查）。
func (e envFlags) map0() map[string]string {
	out := map[string]string{}
	for _, item := range e {
		k, v, ok := strings.Cut(item, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !ok {
			if v2, found := os.LookupEnv(k); found {
				out[k] = v2
			}
			continue
		}
		out[k] = v
	}
	return out
}

func fatal(lg *log.Logger, err error) {
	lg.Printf("启动失败: %v", err)
	os.Exit(1)
}

// Config 的零值表示使用默认值；CLI 的显式零表示关闭，转换成已有的负值哨兵。
func lifecycleDuration(d time.Duration) time.Duration {
	if d == 0 {
		return -time.Nanosecond
	}
	return d
}
