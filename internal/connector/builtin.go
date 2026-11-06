package connector

// 内置连接器写死在代码里。
//
// 为什么不放数据库/配置文件：MVP 阶段「接哪些系统」是产品决定而不是租户配置，
// 让用户自己上传 OAuth 应用需要一套审核流程，先不碰（见 connector.go 注释）。
// 各家的 OAuth 端点也是固定的，配置里只给 client id/secret。

// OAuth 端点（均取自官方文档，见各条注释里的链接）。
const (
	// https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"

	// https://developers.google.com/identity/protocols/oauth2/web-server
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"

	// https://developers.notion.com/docs/authorization
	// notion 的 token 端点用 HTTP Basic 认证，请求体是 JSON，且授权页没有 scope 概念。
	notionAuthorizeURL = "https://api.notion.com/v1/oauth/authorize"
	notionTokenURL     = "https://api.notion.com/v1/oauth/token"
)

// tokenStyle 决定换 token 时请求体怎么编码、客户端凭据怎么带。
type tokenStyle int

const (
	// tokenStyleForm：application/x-www-form-urlencoded + client_id/client_secret 放 body。
	tokenStyleForm tokenStyle = iota
	// tokenStyleNotion：JSON body + Authorization: Basic base64(id:secret)。
	tokenStyleNotion
)

// definition 是内置连接器的完整定义：对外元信息 + 私有 OAuth 细节。
type definition struct {
	Connector
	authorizeURL string
	tokenURL     string
	tokenStyle   tokenStyle
	// extraAuth 是授权页上的额外固定参数（google 要 offline 才给 refresh_token；
	// notion 要求 owner=user）。
	extraAuth map[string]string
	// refreshable 表示该连接器支持 grant_type=refresh_token。
	refreshable bool
	// needsGrantType 表示换 token 时必须显式带 grant_type=authorization_code。
	// google 必填；github 的 web flow 文档里没有这个参数，多传属于无谓的兼容性风险。
	needsGrantType bool
}

// definitions 顺序即 List() 的返回顺序。
//
// scope 依据（都是官方文档里能查到的，不是猜的）：
//   - github  repo + read:user —— 见 docs.github.com/.../scopes-for-oauth-apps：
//     repo 覆盖公开/私有仓库的读写（代码、commit status、webhook 等），
//     read:user 只读用户资料；不申请 admin:org、delete_repo 这类高危 scope。
//   - google  gmail.modify —— 见 developers.google.com/gmail/api/auth/scopes：
//     读信、撰写、发送，且明确不含「绕过回收站的永久删除」；比 mail.google.com/
//     全量范围窄，比 gmail.readonly 够用（属受限范围，授权页会提示）。
//   - notion  无 scope —— 见 developers.notion.com/docs/authorization：授权 URL 的
//     参数只有 client_id/redirect_uri/response_type/owner/state，根本没有 scope；
//     权限靠集成 capabilities + 用户勾选页面。
//   - openai  apikey 型，没有 OAuth，key 由用户贴进来。
//
// 上游信息（Hosts / APIBase）依据：
//   - github  REST 基址 https://api.github.com（官方 scopes 文档里的 curl 示例）。
//   - google  Gmail REST 有 www.googleapis.com（通用根，/gmail/v1/... 可用）与
//     gmail.googleapis.com（官方客户端 DEFAULT_BASE_URL）两个域名；APIBase 取通用根
//     https://www.googleapis.com，两个域名都放进 Hosts 以便 sidecar 校验。
//   - notion  https://api.notion.com（/v1/...），token endpoint 用 HTTP Basic。
//   - openai  https://api.openai.com（/v1/...），见 platform.openai.com/docs/api-reference。
//
// APIBase 统一只到 host、不带 /v1 路径前缀：请求路径由 agent 拼在 sidecar 地址后面，
// sidecar 转发到 APIBase + 路径。
//
// AuthHeader 一律留空：四家的 token 都走 Authorization: Bearer，由 app 的 tokenAdapter
// 兜底成 "Authorization"。Notion 是例外——REST 调用还必须带 Notion-Version（如
// 2022-06-28）与 Content-Type，但那是「Authorization 之外的额外 header」：当前契约的
// AuthHeader 只描述 token header，sidecar 也只注入一个头，这个额外头需要 agent/sidecar
// 另行处理（见汇报里的「局限」）。
var definitions = []definition{
	{
		Connector: Connector{
			Name:        "github",
			DisplayName: "GitHub",
			Kind:        KindOAuth2,
			Scopes:      []string{"repo", "read:user"},
			EnvNames:    []string{"GITHUB_TOKEN"},
			DocsURL:     "https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps",
			Hosts:       []string{"api.github.com"},
			APIBase:     "https://api.github.com",
		},
		authorizeURL: githubAuthorizeURL,
		tokenURL:     githubTokenURL,
		tokenStyle:   tokenStyleForm,
		refreshable:  true,
	},
	{
		Connector: Connector{
			Name:        "google",
			DisplayName: "Google",
			Kind:        KindOAuth2,
			Scopes:      []string{"https://www.googleapis.com/auth/gmail.modify"},
			EnvNames:    []string{"GOOGLE_ACCESS_TOKEN"},
			DocsURL:     "https://developers.google.com/identity/protocols/oauth2/web-server",
			Hosts:       []string{"www.googleapis.com", "gmail.googleapis.com"},
			APIBase:     "https://www.googleapis.com",
		},
		authorizeURL:   googleAuthorizeURL,
		tokenURL:       googleTokenURL,
		tokenStyle:     tokenStyleForm,
		needsGrantType: true,
		extraAuth: map[string]string{
			"access_type": "offline", // 不加这个拿不到 refresh_token
			"prompt":      "consent", // 二次授权也要 refresh_token
		},
		refreshable: true,
	},
	{
		Connector: Connector{
			Name:        "notion",
			DisplayName: "Notion",
			Kind:        KindOAuth2,
			EnvNames:    []string{"NOTION_TOKEN"},
			DocsURL:     "https://developers.notion.com/docs/authorization",
			Hosts:       []string{"api.notion.com"},
			APIBase:     "https://api.notion.com",
		},
		authorizeURL: notionAuthorizeURL,
		tokenURL:     notionTokenURL,
		tokenStyle:   tokenStyleNotion,
		extraAuth:    map[string]string{"owner": "user"},
		refreshable:  true,
	},
	{
		Connector: Connector{
			Name:        "openai",
			DisplayName: "OpenAI",
			Kind:        KindAPIKey,
			EnvNames:    []string{"OPENAI_API_KEY"},
			DocsURL:     "https://platform.openai.com/api-keys",
			Hosts:       []string{"api.openai.com"},
			APIBase:     "https://api.openai.com",
		},
	},
}

// lookup 按名字找内置定义。第二个返回值区分「不存在」和「存在但没配」。
func lookup(name string) (definition, bool) {
	for _, d := range definitions {
		if d.Name == name {
			return d, true
		}
	}
	return definition{}, false
}

// clientCreds 返回该连接器配置的 OAuth 应用凭据（空表示没配）。
func (m *manager) clientCreds(name string) (id, secret string) {
	switch name {
	case "github":
		return m.cfg.GitHubClientID, m.cfg.GitHubClientSecret
	case "google":
		return m.cfg.GoogleClientID, m.cfg.GoogleClientSecret
	case "notion":
		return m.cfg.NotionClientID, m.cfg.NotionClientSecret
	}
	return "", ""
}
