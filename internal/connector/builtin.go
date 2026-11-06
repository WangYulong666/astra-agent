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
//   - github  repo + read:user —— repo 覆盖读写代码仓库（agent 最常用的是读代码/提 PR），
//     read:user 只为读用户资料；不申请 admin:org、delete_repo 这类高危 scope。
//   - google  gmail.modify —— 单一 scope 覆盖读信、打标签、发信草稿，
//     比 gmail（全量，含永久删除）窄，比 readonly 够用。
//   - notion  无 scope —— Notion 的权限模型是「用户在授权页勾选要给哪些页面」+ 集成
//     capabilities，OAuth 授权 URL 根本不接受 scope 参数（见官方 authorization 文档）。
//   - openai  apikey 型，没有 OAuth，key 由用户贴进来。
var definitions = []definition{
	{
		Connector: Connector{
			Name:        "github",
			DisplayName: "GitHub",
			Kind:        KindOAuth2,
			Scopes:      []string{"repo", "read:user"},
			EnvNames:    []string{"GITHUB_TOKEN"},
			DocsURL:     "https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps",
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
