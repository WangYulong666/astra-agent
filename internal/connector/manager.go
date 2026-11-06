package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// callbackPath 是 OAuth 回调地址。Api 层注册的路由必须和这里一致，
	// 否则用户从授权页回来会 404（配置了 ConnectorRedirectURL 时以配置为准）。
	callbackPath = "/v1/connectors/callback"
	// tokenTimeout 是换 token / 刷 token 的 HTTP 超时。
	tokenTimeout = 20 * time.Second
	// refreshSkew 提前刷新窗口：expires 前 60 秒就当作已过期，
	// 避免 token 在请求飞行途中刚好失效。
	refreshSkew = 60 * time.Second
)

// ErrInjectProxyUnimplemented 是 v3 之前 proxy 模式的「诚实回复」。
// proxy 注入已在 Inject 里落地，本错误不再由 Inject 返回；保留变量是因为
// 其它包（如 runner 的单测）仍引用它作为「注入失败」的哨兵，删掉会破坏编译。
var ErrInjectProxyUnimplemented = errors.New(
	"connector: proxy 注入尚未实现——egress 代理还没接 token 替换，注入了也没有任何效果；" +
		"临时办法是把 CONNECTOR_INJECT_MODE 设为 env（token 会进沙箱环境，有泄漏风险），或先不注入")

// ErrProxyUnavailable 表示 proxy 模式要用的出网代理（sidecar）没启用：
// 此时不能给一个连不上的假地址，也不能静默返回空 map（runner 会当成成功）。
var ErrProxyUnavailable = errors.New("connector: 出网代理未启用，proxy 注入不可用（SidecarBase 为空）")

type manager struct {
	cfg     Config
	enabled bool
	vault   *vault
	// stateKey 由 VaultKey 派生，不单独配置。
	stateKey  []byte
	injectMod InjectMode
	http      *http.Client
	// tokenURLs 默认来自 definitions；单测里覆盖成 httptest 地址。
	tokenURLs map[string]string
	// tokenCache / cacheMu 是 TokenFor 的短期缓存（实现见 ext_stub.go）。
	cacheMu    sync.Mutex
	tokenCache map[string]tokenCacheEntry
}

// Open 装配连接器与保管库。
//
// 失败即「能力不存在」：调用方（internal/app）拿到 error 会把它降级为 nil，
// 平台照常启动。没有主密钥时绝不能退化成明文存储。
func Open(cfg Config) (Manager, error) {
	m := &manager{
		cfg:        cfg,
		http:       &http.Client{Timeout: tokenTimeout},
		injectMod:  cfg.InjectMode,
		tokenURLs:  make(map[string]string, len(definitions)),
		tokenCache: make(map[string]tokenCacheEntry),
	}
	for _, d := range definitions {
		if d.tokenURL != "" {
			m.tokenURLs[d.Name] = d.tokenURL
		}
	}

	if !cfg.Enabled {
		// 关掉就是关掉：所有方法回 ErrDisabled，连回调都不该有副作用。
		return m, nil
	}
	if m.injectMod == "" {
		m.injectMod = InjectProxy
	}
	if m.injectMod != InjectProxy && m.injectMod != InjectEnv {
		return nil, fmt.Errorf("connector: 未知的 CONNECTOR_INJECT_MODE=%q（只认 %q 或 %q）",
			m.injectMod, InjectProxy, InjectEnv)
	}

	key, err := decodeVaultKey(cfg.VaultKey)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSpace(cfg.VaultPath)
	if path == "" {
		path = defaultVaultPath()
	}
	v, err := openVault(path, key)
	if err != nil {
		return nil, err
	}
	m.vault = v
	m.stateKey = deriveStateKey(key)
	m.enabled = true
	return m, nil
}

// decodeVaultKey 解 base64 主密钥。四种编码都试一遍：
// 运维从 `openssl rand -base64 32` 拷来的通常带 padding，
// 从 URL query/密钥管理系统里拿的常常是 raw-url 形式。
func decodeVaultKey(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, ErrDisabled
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		if key, err := enc.DecodeString(s); err == nil {
			if len(key) != 32 {
				return nil, fmt.Errorf("connector: VAULT_KEY 解码后必须 32 字节（AES-256），当前 %d 字节", len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("connector: VAULT_KEY 不是合法 base64（期望 `openssl rand -base64 32` 的输出）")
}

func defaultVaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "vault.json"
	}
	return filepath.Join(home, ".astra", "vault.json")
}

func (m *manager) Enabled() bool {
	return m != nil && m.enabled && m.vault != nil
}

// cloneConnector 拷贝切片字段，避免调用方（HTTP 层 / app 的 tokenAdapter）
// 改到包级 definitions。
func cloneConnector(c Connector) Connector {
	c.Hosts = append([]string(nil), c.Hosts...)
	c.Scopes = append([]string(nil), c.Scopes...)
	c.EnvNames = append([]string(nil), c.EnvNames...)
	return c
}

func (m *manager) List() []Connector {
	out := make([]Connector, 0, len(definitions))
	for _, d := range definitions {
		out = append(out, cloneConnector(d.Connector))
	}
	return out
}

func (m *manager) ListCredentials(ctx context.Context, tenantID, userID string) ([]Credential, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	return m.vault.list(tenantID, userID)
}

func (m *manager) Delete(ctx context.Context, tenantID, userID, name string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	if _, ok := lookup(name); !ok {
		return fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
	}
	if err := m.vault.delete(tenantID, userID, name); err != nil {
		return err
	}
	m.invalidateTokenCache(tenantID, userID, name)
	return nil
}

// PutKey 存一个 API key（只有 apikey 型连接器能走这条路）。
func (m *manager) PutKey(ctx context.Context, tenantID, userID, name, key string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	def, ok := lookup(name)
	if !ok {
		return fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
	}
	if def.Kind != KindAPIKey {
		return fmt.Errorf("%w: %s 是 OAuth 连接器，请走 /connect 授权而不是贴 key", ErrUnsupported, name)
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("connector: key 不能为空")
	}
	if err := m.vault.set(Credential{
		TenantID: tenantID,
		UserID:   userID,
		Name:     name,
		Kind:     def.Kind,
		Token:    strings.TrimSpace(key),
		LinkedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	m.invalidateTokenCache(tenantID, userID, name)
	return nil
}

// Inject 返回要注入沙箱的环境变量。
//
// 两种模式语义差别很大：
//   - proxy（默认）：只给「本地 sidecar 基址」，不含任何 token。token 留在控制面，
//     由 egress sidecar 在转发请求时按用户取用注入。env 名从 EnvNames[0] 推导，
//     见 apiBaseEnvName。这样 agent 即便打印全部环境变量也拿不到凭据。
//   - env：把明文 token 放进环境变量（有泄漏风险，仅可信 agent + 短生命周期
//     token 时用），且每次打一条醒目告警，不静默降级。
func (m *manager) Inject(ctx context.Context, tenantID, userID string, names []string) (map[string]string, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	if len(names) == 0 {
		return map[string]string{}, nil
	}
	if m.injectMod == InjectProxy {
		return m.injectProxyEnv(names)
	}

	// InjectEnv 明文分支保持原样：token 进沙箱 + 醒目告警。
	env := make(map[string]string, len(names))
	injected := make([]string, 0, len(names))
	for _, name := range names {
		def, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
		}
		if len(def.EnvNames) == 0 {
			return nil, fmt.Errorf("connector: %s 没有定义注入的环境变量名", name)
		}
		cred, err := m.credential(ctx, tenantID, userID, name)
		if err != nil {
			return nil, err
		}
		envName := def.EnvNames[0]
		env[envName] = cred.Token
		injected = append(injected, envName)
	}
	log.Printf("[安全告警] 连接器凭据以明文环境变量注入沙箱：%s（tenant=%s user=%s）——"+
		"沙箱内任何代码（包括 agent 自己）都能读取并外传，也可能出现在日志/回放里。"+
		"仅在可信 agent + 短生命周期 token 下使用；正确做法是 proxy 模式（token 不进沙箱）。",
		strings.Join(injected, ","), tenantID, userID)
	return env, nil
}

// injectProxyEnv 给沙箱的是 sidecar 基址而不是 token。
//
// 地址形如 http://<SidecarBase>/_connector/<name>（SidecarBase 是 host:port），
// 与 internal/egress 的 sidecarPath 约定一致；sidecar 转发时按 name 取 TokenFor
// 注入 header 再打到 Connector.APIBase。这里刻意不读 token、也不检查用户是否已连接：
// 连接与否由 sidecar 的 TokenFor 决定（未授权时拒绝转发）。
func (m *manager) injectProxyEnv(names []string) (map[string]string, error) {
	base := strings.TrimSuffix(strings.TrimSpace(m.cfg.SidecarBase), "/")
	if base == "" {
		// 代理没启用：明确报错，绝不给一个连不上的假地址，也不静默返回空 map。
		return nil, ErrProxyUnavailable
	}
	env := make(map[string]string, len(names))
	for _, name := range names {
		def, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("%w: 未知连接器 %q", ErrNotFound, name)
		}
		if len(def.EnvNames) == 0 {
			return nil, fmt.Errorf("connector: %s 没有定义注入的环境变量名", name)
		}
		if def.APIBase == "" || len(def.Hosts) == 0 {
			return nil, fmt.Errorf("connector: %s 未定义 Hosts/APIBase，无法走 proxy 注入", name)
		}
		env[apiBaseEnvName(def.EnvNames[0])] = "http://" + base + "/_connector/" + name
	}
	return env, nil
}

// apiBaseEnvName 从 token 环境变量名推导 *_API_BASE 名。规则写死在这里，改内置
// EnvNames 时必须同步本函数与 TestAPIBaseEnvNameDerivation：
//  1. 去掉 token/key 类后缀，长后缀优先（否则 GOOGLE_ACCESS_TOKEN 会被 _TOKEN
//     切成 GOOGLE_ACCESS、OPENAI_API_KEY 会被 _KEY 切成 OPENAI_API）；顺序固定为
//     _ACCESS_TOKEN → _API_KEY → _TOKEN → _KEY。
//  2. 剩下的前缀加 _API_BASE。
//
// 例：GITHUB_TOKEN → GITHUB_API_BASE；GOOGLE_ACCESS_TOKEN → GOOGLE_API_BASE；
// NOTION_TOKEN → NOTION_API_BASE；OPENAI_API_KEY → OPENAI_API_BASE。
// 没匹配到后缀时保守地直接加 _API_BASE（仍是确定、可用的变量名）。
func apiBaseEnvName(envName string) string {
	for _, suffix := range []string{"_ACCESS_TOKEN", "_API_KEY", "_TOKEN", "_KEY"} {
		if strings.HasSuffix(envName, suffix) {
			return strings.TrimSuffix(envName, suffix) + "_API_BASE"
		}
	}
	return envName + "_API_BASE"
}

// credential 取一条明文凭据，快过期时先刷新。
func (m *manager) credential(ctx context.Context, tenantID, userID, name string) (Credential, error) {
	cred, err := m.vault.get(tenantID, userID, name)
	if err != nil {
		return Credential{}, err
	}
	if cred.Expires.IsZero() || time.Now().Before(cred.Expires.Add(-refreshSkew)) {
		return cred, nil
	}
	if cred.Refresh == "" {
		// 没有 refresh_token 就无从刷新：把原 token 交给调用方，由上游 API 自己报 401。
		// 这里不假装成功刷新，也不删凭据——用户重新授权即可覆盖。
		log.Printf("connector: %s 的 token 已过期且没有 refresh_token（tenant=%s user=%s），需要重新授权",
			name, tenantID, userID)
		return cred, nil
	}
	refreshed, err := m.refresh(ctx, tenantID, userID, name, cred)
	if err != nil {
		return Credential{}, err
	}
	return refreshed, nil
}

// redirectURI 解析回调地址：handler 传的优先（可能是专门的公网回调域名），
// 为空则从 BaseURL 拼 —— 两边必须算出同一个值，否则 OAuth 换 token 会被拒。
func (m *manager) redirectURI(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	base := strings.TrimRight(strings.TrimSpace(m.cfg.BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + callbackPath
}
