package egress

import (
	"net/url"
	"strings"
	"testing"
)

func TestEnvForCarriesSandboxIdentity(t *testing.T) {
	env := EnvFor("127.0.0.1:17890", "sb-123", "nonce-abc")
	if len(env) != 6 {
		t.Fatalf("应有大小写共 6 个代理变量，实际 %d: %v", len(env), env)
	}
	u, err := url.Parse(env["HTTPS_PROXY"])
	if err != nil {
		t.Fatalf("代理地址解析失败: %v", err)
	}
	if u.Host != "127.0.0.1:17890" {
		t.Fatalf("host 应为代理地址，实际 %q", u.Host)
	}
	if u.User.Username() != "sb-123" {
		t.Fatalf("userinfo 应带 sandboxID，实际 %q", u.User.Username())
	}
	if pw, _ := u.User.Password(); pw != "nonce-abc" {
		t.Fatalf("userinfo 应带 nonce，实际 %q", pw)
	}
	// 默认严格：不许偷偷放行 localhost
	if _, ok := env["NO_PROXY"]; ok {
		t.Fatal("EnvFor 默认不应设置 NO_PROXY")
	}
	for k, v := range env {
		if !strings.HasPrefix(v, "http://sb-123:nonce-abc@") {
			t.Errorf("%s 缺归属信息: %q", k, v)
		}
	}
}

func TestEnvForWithBypassIsExplicitAndNarrow(t *testing.T) {
	env := EnvForWithBypass("127.0.0.1:17890", "sb-1", "n1", "127.0.0.1:7788")
	if env["NO_PROXY"] != "127.0.0.1:7788" || env["no_proxy"] != "127.0.0.1:7788" {
		t.Fatalf("NO_PROXY 应精确等于网关地址，实际 %v", env)
	}
	if _, ok := env["HTTP_PROXY"]; !ok {
		t.Fatal("加例外不能把代理本身弄丢")
	}
	// bypass 为空时退回严格模式
	strict := EnvForWithBypass("127.0.0.1:17890", "sb-1", "n1", "")
	if _, ok := strict["NO_PROXY"]; ok {
		t.Fatal("bypass 为空不应设 NO_PROXY")
	}
	// 代理没启用（地址空）时不能注入半套变量
	if got := EnvForWithBypass("", "sb-1", "n1", "127.0.0.1:7788"); len(got) != 0 {
		t.Fatalf("代理未启用应返回空 map，实际 %v", got)
	}
}
