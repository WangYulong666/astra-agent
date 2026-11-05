// Package llmwire 把「会话级虚拟 key」送进沙箱：签发 token，并把它和网关地址
// 组装成 dsh 认的那两个环境变量。
//
// 为什么需要这一层：一期的做法是把真 DEEPSEEK_API_KEY 直接塞进沙箱进程的 env，
// agent 一句 `env` 就能读到、模型也能把它写进回答里。现在沙箱里只放 2h 的虚拟
// key，真 key 留在控制面（网关进程内存），凭据边界才真正成立。
//
// 为什么只碰环境变量、不碰 dsh 源码：dsh 的 deepseek 插件本来就支持用
// DEEPSEEK_BASE_URL 覆盖端点（`lib/index.js` 里 `config.baseURL ?? environment?.get(BASE_URL_ENV)?.value`），
// 凭据变量名由 api-key 插件的 apiKeyEnv 决定，默认 DEEPSEEK_API_KEY。
// 也就是说，注入两个 env 就够了，不需要维护一份 per-session 的 profile patch。
package llmwire

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/runner"
)

const (
	// EnvAPIKey 沙箱里放虚拟 key 的变量名。
	//
	// 这个名字不是我们定的：dsh-llm-deepseek-api-key 插件的 apiKeyEnv 默认值就是它，
	// 改了 dsh 就找不到凭据（会以 MISSING_CREDENTIAL 拒绝发起请求）。
	EnvAPIKey = "DEEPSEEK_API_KEY"

	// EnvBaseURL 覆盖 dsh 的端点根地址。
	//
	// dsh 解析顺序是 config.baseURL > 该环境变量 > 官方 https://api.deepseek.com/anthropic。
	// 我们没有在 patch 里写 config.baseURL（它是部署期才知道的值），所以实际生效的就是这一条。
	EnvBaseURL = "DEEPSEEK_BASE_URL"

	// DefaultBasePath 网关在控制面上的挂载前缀，见 internal/api/platform_routes.go
	// 的 `POST /v1/llm/{path...}`。
	DefaultBasePath = "/v1/llm"

	// anthropicSuffix 拼在 basePath 之后。
	//
	// 为什么要有这一层：dsh 说的是 Anthropic Messages 协议，请求路径是
	// `{baseURL}/v1/messages`，而上游 DeepSeek 的 Anthropic 入口在 /anthropic 下。
	// 把网关地址设成 `{网关}/v1/llm/anthropic`，dsh 拼出来的
	// `/v1/llm/anthropic/v1/messages` 剥掉 `/v1/llm` 前缀后正好命中上游。
	anthropicSuffix = "/anthropic"
)

// EnvKeys 返回本包会注入的全部环境变量名。
//
// 存在的意义是给上层测试、启动日志、文档引用同一份事实：变量名只在这个包里
// 出现一次，外面再写字符串字面量就有写歪的机会（写歪了 dsh 不会报错，只会
// 悄悄连去官方端点、用真 key 失败）。
func EnvKeys() []string {
	return []string{EnvAPIKey, EnvBaseURL}
}

// Config 构造 Issuer 的入参。
type Config struct {
	// Auth 用于签虚拟 key。必填：没有它就只能回落到转发真 key。
	Auth *auth.Service

	// PublicURL 是 agent 视角可达的网关根地址，如 http://127.0.0.1:7788
	// 或 k8s 里的 http://astra-api:8080。
	//
	// 它和控制面自己监听的地址不是一回事（容器内 0.0.0.0 对 agent 没有意义），
	// 所以必须显式给。为空即可用性配置错误——绝不猜一个，猜错的表现是
	// 「agent 起来了但永远连不上模型」，比启动时报错难查得多。
	PublicURL string

	// BasePath 网关挂载前缀，默认 "/v1/llm"。
	BasePath string

	// TTL 虚拟 key 有效期，默认 auth.DefaultVirtualKeyTTL（2h）。
	TTL time.Duration

	// Logger 可空。只用来打配置摘要，不会打印 token 本身。
	Logger *log.Logger
}

// Issuer 实现 runner.CredentialIssuer。
type Issuer struct {
	auth *auth.Service
	// baseURL 是归一化后的完整端点（已含 basePath 与 /anthropic），
	// 签发时原样塞进 EnvBaseURL，避免每个会话重复拼串。
	baseURL string
	ttl     time.Duration
}

// 编译期确认契约：接口变了这里会先红，而不是等到 manager 装配时才发现。
var _ runner.CredentialIssuer = (*Issuer)(nil)

// New 构造 Issuer，并在这里把 PublicURL 一次性校验干净。
//
// 为什么校验放在构造期而不是签发期：端点写错是部署期错误，应该在进程启动时就
// 炸掉。等到第一个会话才报，等于把配置错误伪装成「运行时偶发故障」。
func New(cfg Config) (*Issuer, error) {
	if cfg.Auth == nil {
		return nil, errors.New("llmwire: Config.Auth 不能为空——签发虚拟 key 必须走 auth.Service")
	}
	base, err := buildBaseURL(cfg.PublicURL, cfg.BasePath)
	if err != nil {
		return nil, err
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		// 与网关/文档保持同一个默认值，不在本包另立一个 2h 常量。
		ttl = auth.DefaultVirtualKeyTTL
	}
	if cfg.Logger != nil {
		// 只打端点与有效期：token 是凭据，一旦进日志就等于漏过一次。
		cfg.Logger.Printf("LLM 虚拟 key 注入已启用：沙箱内 %s=%s，有效期 %s", EnvBaseURL, base, ttl)
	}
	return &Issuer{auth: cfg.Auth, baseURL: base, ttl: ttl}, nil
}

// IssueForSession 为一个会话签发虚拟 key 并返回要合并进 AgentSpec.Env 的覆盖项。
//
// ctx 目前用不上（签发是纯本地 HS256 签名，不查库、不发网络），保留它是为了
// 对齐 runner.CredentialIssuer：将来若要在签发前查配额或会话状态，调用方不必改签名。
func (i *Issuer) IssueForSession(ctx context.Context, scope runner.SessionScope) (map[string]string, time.Time, error) {
	_ = ctx

	// 三者的非空校验由 auth.IssueVirtualKey 负责，这里不再重复：重复实现只会
	// 在将来某天和 auth 的规则悄悄分叉。
	token, exp, err := i.auth.IssueVirtualKey(scope.TenantID, scope.UserID, scope.SessionID, i.ttl)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("llmwire: 为会话 %q 签发虚拟 key 失败: %w", scope.SessionID, err)
	}
	return map[string]string{
		EnvAPIKey:  token,
		EnvBaseURL: i.baseURL,
	}, exp, nil
}

// buildBaseURL 校验 PublicURL 并拼出 dsh 要的完整端点。
//
// 校验项不是我们自己加的洁癖，而是 dsh 的硬约束：它在构造 adapter 时会用
// `new URL(baseURL)` 检查协议为 http(s)、且没有用户名密码/query/fragment，
// 否则直接抛 "Messages baseURL must be an HTTP(S) root without credentials, query, or fragment"。
// 与其让沙箱在 session/new 之后才崩（那时沙箱已经起来了，还得清理），
// 不如在控制面启动时就拒绝。
func buildBaseURL(publicURL, basePath string) (string, error) {
	raw := strings.TrimSpace(publicURL)
	if raw == "" {
		return "", errors.New("llmwire: Config.PublicURL 不能为空——它必须是 agent 视角可达的网关根地址" +
			"（本机如 http://127.0.0.1:7788，k8s 里写 service DNS 或对外域名）")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("llmwire: Config.PublicURL %q 不是合法 URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("llmwire: Config.PublicURL 的协议必须是 http 或 https，实际是 %q", u.Scheme)
	}
	if u.Host == "" {
		// `http:///v1/llm` 这种能通过 url.Parse，但 dsh 拼出来的请求没有主机名。
		return "", fmt.Errorf("llmwire: Config.PublicURL %q 缺少主机名", raw)
	}
	if u.User != nil {
		return "", errors.New("llmwire: Config.PublicURL 不能带用户名/密码（dsh 会拒绝带 credentials 的 baseURL）")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("llmwire: Config.PublicURL 不能带 query（dsh 会拒绝带 query 的 baseURL）")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("llmwire: Config.PublicURL 不能带 fragment（dsh 会拒绝带 fragment 的 baseURL）")
	}

	bp, err := normalizeBasePath(basePath)
	if err != nil {
		return "", err
	}

	// 尾斜杠必须在这里抹掉：PublicURL 写 "http://host/" 时 naive 拼接会得到
	// "http://host//v1/llm/anthropic"，dsh 不报错但请求路径会多一个空段，
	// 上游回 404——这种错看起来像「网关没挂载」，排查要绕一大圈。
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = "" // Path 已是解码形态，留着 RawPath 会让 String 走另一条编码分支
	return u.String() + bp + anthropicSuffix, nil
}

// normalizeBasePath 归一化挂载前缀。
func normalizeBasePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return DefaultBasePath, nil
	}
	if !strings.HasPrefix(p, "/") {
		// 不自动补前导斜杠：`v1/llm` 拼出来是 `http://hostv1/llm`，
		// 结果是一个看起来像域名、实际永远解析不到的地址。宁可当场拒绝。
		return "", fmt.Errorf("llmwire: Config.BasePath 必须以 / 开头（如 %q），实际是 %q", DefaultBasePath, p)
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", fmt.Errorf("llmwire: Config.BasePath 不能是根路径 /——网关挂在 %s 之下", DefaultBasePath)
	}
	return p, nil
}
