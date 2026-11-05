package llmwire

// 本包的测试分两类：
//   - 构造期校验：端点写错必须在 New 就失败，而不是等沙箱起来之后由 dsh 抛错。
//   - 签发结果形态：环境变量的键名、值的形状、以及 token 能否被网关验过。
//
// 用嵌入 nil 的假 store 而不是内存库：IssueForSession 走的是纯签名路径，一个
// 库访问都不该发生。嵌入接口后任何一次意外的库调用都会立刻 nil panic，
// 等于把「签发不应碰库」变成一条自动执行的断言。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/auth"
	"github.com/WangYulong666/astra-agent/internal/runner"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// stubAdminStore 只满足 auth.New 的「非 nil」检查。
// 所有方法都从嵌入的 nil 接口提升而来，真被调用就 panic——这正是我们想要的信号。
type stubAdminStore struct{ store.AdminStore }

func newService(t *testing.T) *auth.Service {
	t.Helper()
	svc, err := auth.New(auth.Config{
		Store: stubAdminStore{},
		// 32 字节够长，且固定值让测试不依赖随机源。
		Secret: []byte("llmwire-test-secret-0123456789ab"),
	})
	if err != nil {
		t.Fatalf("构造 auth.Service 失败: %v", err)
	}
	return svc
}

func TestNewRejectsBadPublicURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		basePath  string
		wantErr   string
	}{
		{
			name:      "空串",
			publicURL: "",
			wantErr:   "PublicURL 不能为空",
		},
		{
			name:      "只有空白",
			publicURL: "   ",
			wantErr:   "PublicURL 不能为空",
		},
		{
			name:      "带 query",
			publicURL: "http://127.0.0.1:7788?token=x",
			wantErr:   "不能带 query",
		},
		{
			name:      "只有问号的空 query",
			publicURL: "http://127.0.0.1:7788?",
			wantErr:   "不能带 query",
		},
		{
			name:      "带 userinfo",
			publicURL: "http://user:pw@127.0.0.1:7788",
			wantErr:   "不能带用户名/密码",
		},
		{
			name:      "非 http scheme",
			publicURL: "ftp://gw.example.com",
			wantErr:   "协议必须是 http 或 https",
		},
		{
			name:      "缺 scheme",
			publicURL: "127.0.0.1:7788",
			wantErr:   "不是合法 URL",
		},
		{
			name:      "相对路径",
			publicURL: "/v1/llm",
			wantErr:   "协议必须是 http 或 https",
		},
		{
			name:      "缺主机名",
			publicURL: "http:///v1/llm",
			wantErr:   "缺少主机名",
		},
		{
			name:      "带 fragment",
			publicURL: "http://127.0.0.1:7788/x#frag",
			wantErr:   "不能带 fragment",
		},
		{
			name:      "BasePath 缺前导斜杠",
			publicURL: "http://127.0.0.1:7788",
			basePath:  "v1/llm",
			wantErr:   "BasePath 必须以 / 开头",
		},
		{
			name:      "BasePath 是根路径",
			publicURL: "http://127.0.0.1:7788",
			basePath:  "/",
			wantErr:   "BasePath 不能是根路径",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Auth: newService(t), PublicURL: tc.publicURL, BasePath: tc.basePath})
			if err == nil {
				t.Fatalf("期望 New 报错（%s），实际通过", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}
}

func TestNewRequiresAuth(t *testing.T) {
	// 没有 Auth 就只能回落去转发真 key，那正是本包要消灭的行为，必须硬失败。
	_, err := New(Config{PublicURL: "http://127.0.0.1:7788"})
	if err == nil || !strings.Contains(err.Error(), "Config.Auth 不能为空") {
		t.Fatalf("缺少 Auth 时应报错，实际: %v", err)
	}
}

func TestBaseURLNormalization(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		basePath  string
		want      string
	}{
		{
			name:      "默认挂载前缀",
			publicURL: "http://127.0.0.1:7788",
			want:      "http://127.0.0.1:7788/v1/llm/anthropic",
		},
		{
			name:      "PublicURL 尾斜杠",
			publicURL: "http://127.0.0.1:7788/",
			want:      "http://127.0.0.1:7788/v1/llm/anthropic",
		},
		{
			name:      "PublicURL 多个尾斜杠",
			publicURL: "http://127.0.0.1:7788///",
			want:      "http://127.0.0.1:7788/v1/llm/anthropic",
		},
		{
			name:      "PublicURL 带路径前缀 + 尾斜杠",
			publicURL: "https://gw.example.com/astra/",
			want:      "https://gw.example.com/astra/v1/llm/anthropic",
		},
		{
			name:      "BasePath 尾斜杠",
			publicURL: "http://127.0.0.1:7788",
			basePath:  "/v1/llm/",
			want:      "http://127.0.0.1:7788/v1/llm/anthropic",
		},
		{
			name:      "自定义 BasePath",
			publicURL: "http://astra-api:8080",
			basePath:  "/api/llm",
			want:      "http://astra-api:8080/api/llm/anthropic",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			iss, err := New(Config{Auth: newService(t), PublicURL: tc.publicURL, BasePath: tc.basePath})
			if err != nil {
				t.Fatalf("New 失败: %v", err)
			}
			if iss.baseURL != tc.want {
				t.Fatalf("端点应为 %q，实际 %q", tc.want, iss.baseURL)
			}
		})
	}
}

func TestIssueForSessionEnvShape(t *testing.T) {
	const ttl = 30 * time.Minute
	iss, err := New(Config{
		Auth:      newService(t),
		PublicURL: "http://127.0.0.1:7788/",
		TTL:       ttl,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	before := time.Now()
	env, exp, err := iss.IssueForSession(context.Background(), runner.SessionScope{
		SessionID: "sess-1", TenantID: "ten-1", UserID: "usr-1",
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	after := time.Now()

	// 只允许这两个键：多塞一个是无意的行为变更，少一个 dsh 就用不上网关。
	wantKeys := EnvKeys()
	if len(env) != len(wantKeys) {
		t.Fatalf("env 应恰好有 %d 个键 %v，实际 %d 个: %v", len(wantKeys), wantKeys, len(env), env)
	}
	for _, k := range wantKeys {
		if _, ok := env[k]; !ok {
			t.Fatalf("env 缺少键 %s，实际: %v", k, env)
		}
	}

	token := env[EnvAPIKey]
	// T2 验收标准的字面要求：JWT 三段 base64url、以 ey 开头、没有 sk- 前缀。
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("虚拟 key 应是三段 JWT，实际 %d 段: %q", len(parts), token)
	}
	if !strings.HasPrefix(token, "ey") {
		t.Fatalf("JWT 应以 ey 开头，实际: %q", token)
	}
	if strings.Contains(token, "sk-") {
		t.Fatalf("虚拟 key 里不该出现真 key 的特征 sk-: %q", token)
	}

	if got := env[EnvBaseURL]; !strings.HasSuffix(got, "/v1/llm/anthropic") {
		t.Fatalf("DEEPSEEK_BASE_URL 应以 /v1/llm/anthropic 结尾，实际: %q", got)
	}
	if got, want := env[EnvBaseURL], "http://127.0.0.1:7788/v1/llm/anthropic"; got != want {
		t.Fatalf("DEEPSEEK_BASE_URL 应为 %q，实际 %q", want, got)
	}

	// 有效期要落在 [before+ttl, after+ttl] 这个窄区间里：既验 TTL 生效，
	// 也验返回的 expiresAt 与 token 里的 exp 是同一时刻。
	if exp.Before(before.Add(ttl)) || exp.After(after.Add(ttl)) {
		t.Fatalf("expiresAt 应约等于 now+%s，实际 %s", ttl, exp)
	}
}

func TestIssueForSessionTokenVerifies(t *testing.T) {
	svc := newService(t)
	iss, err := New(Config{Auth: svc, PublicURL: "http://astra-api:8080"})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	scope := runner.SessionScope{SessionID: "sess-verify", TenantID: "ten-verify", UserID: "usr-verify"}
	env, exp, err := iss.IssueForSession(context.Background(), scope)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 这一条是整条链路的契约：沙箱里拿到的串，网关必须认，且归属必须与原会话一致，
	// 否则计费会记到别人头上。
	claims, err := svc.VerifyVirtualKey(env[EnvAPIKey])
	if err != nil {
		t.Fatalf("网关验签失败: %v", err)
	}
	if claims.TenantID != scope.TenantID || claims.UserID != scope.UserID || claims.SessionID != scope.SessionID {
		t.Fatalf("claims 归属不一致: %+v，期望 %+v", claims, scope)
	}
	if claims.Scope != auth.VirtualKeyScope {
		t.Fatalf("scope 应为 %q，实际 %q", auth.VirtualKeyScope, claims.Scope)
	}
	// 默认 TTL 是 2h，这里只做量级校验：换算成时长后应接近 2 小时（允许数秒误差）。
	if d := claims.ExpiresAt.Sub(claims.IssuedAt); d != auth.DefaultVirtualKeyTTL {
		t.Fatalf("token 内有效期应为默认 %s，实际 %s", auth.DefaultVirtualKeyTTL, d)
	}
	// JWT 的 NumericDate 精度只到秒，所以按秒比较：毫秒以下必然被截断，
	// 直接 Equal 会永远失败。
	if claims.ExpiresAt.Unix() != exp.Unix() {
		t.Fatalf("返回的 expiresAt 与 token 内 exp 不一致: %s vs %s", exp, claims.ExpiresAt)
	}
}

func TestIssueForSessionRejectsIncompleteScope(t *testing.T) {
	iss, err := New(Config{Auth: newService(t), PublicURL: "http://127.0.0.1:7788"})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	// 无租户/用户的会话只能出现在兼容模式里，那种会话不该拿到虚拟 key——
	// 否则计费与归属会落到空字符串上。
	if _, _, err := iss.IssueForSession(context.Background(), runner.SessionScope{SessionID: "sess-only"}); err == nil {
		t.Fatal("缺少租户与用户时应拒绝签发")
	}
}

func TestEnvKeysIsCopy(t *testing.T) {
	// 返回副本：调用方改了返回值不该污染下一个调用方拿到的键名。
	got := EnvKeys()
	got[0] = "MUTATED"
	if EnvKeys()[0] != EnvAPIKey {
		t.Fatal("EnvKeys 返回的切片被共享了，应每次返回副本")
	}
}
