package egress

import (
	"context"
	"reflect"
	"testing"
)

func TestDecideHostPrivateWins(t *testing.T) {
	// 即使策略写了 allow *，私网也必须被拦——这是 SSRF 防线，不是白名单的一部分。
	pol := Policy{
		Default:      Allow,
		AllowPrivate: false,
		Rules:        []Rule{{Pattern: "*", Decision: Allow}},
	}
	private := []string{
		"10.1.2.3",
		"172.20.0.1",
		"192.168.1.1",
		"127.0.0.1",
		"169.254.169.254",
		"10.1.2.3:8080",
	}
	for _, host := range private {
		dec, reason := decideHost(pol, host)
		if dec != Deny || reason != "private-address" {
			t.Errorf("decideHost(%q) = (%s, %s), want (deny, private-address)", host, dec, reason)
		}
	}

	// 打开 AllowPrivate 后同一份策略应放行。
	pol.AllowPrivate = true
	if dec, reason := decideHost(pol, "10.1.2.3"); dec != Allow || reason != "rule:*" {
		t.Errorf("AllowPrivate=true 时 = (%s, %s), want (allow, rule:*)", dec, reason)
	}
	if dec, _ := decideHost(pol, "169.254.169.254"); dec != Allow {
		t.Error("AllowPrivate=true 应按规则放行（是否允许由租户显式决定）")
	}
}

func TestDecideHostFirstMatchWins(t *testing.T) {
	host := "api.example.com"
	allowFirst := Policy{
		Default: Deny,
		Rules: []Rule{
			{Pattern: "*.example.com", Decision: Allow},
			{Pattern: "api.example.com", Decision: Deny},
		},
	}
	if dec, reason := decideHost(allowFirst, host); dec != Allow || reason != "rule:*.example.com" {
		t.Errorf("第一条 allow 应命中: (%s, %s)", dec, reason)
	}

	denyFirst := Policy{
		Default: Allow,
		Rules: []Rule{
			{Pattern: "api.example.com", Decision: Deny},
			{Pattern: "*.example.com", Decision: Allow},
		},
	}
	if dec, reason := decideHost(denyFirst, host); dec != Deny || reason != "rule:api.example.com" {
		t.Errorf("第一条 deny 应命中: (%s, %s)", dec, reason)
	}
}

func TestDecideHostAskFallsBackToDeny(t *testing.T) {
	pol := Policy{
		Default: Deny,
		Rules:   []Rule{{Pattern: "ask.example.com", Decision: Ask}},
	}
	dec, reason := decideHost(pol, "ask.example.com")
	if dec != Deny || reason != "ask-unsupported" {
		t.Errorf("Ask 规则 = (%s, %s), want (deny, ask-unsupported)", dec, reason)
	}

	defAsk := Policy{Default: Ask}
	if dec, reason := decideHost(defAsk, "whatever.example"); dec != Deny || reason != "ask-unsupported" {
		t.Errorf("Ask 默认 = (%s, %s), want (deny, ask-unsupported)", dec, reason)
	}
}

func TestDecideHostDefault(t *testing.T) {
	if dec, reason := decideHost(Policy{Default: Allow}, "example.com"); dec != Allow || reason != "default" {
		t.Errorf("默认 allow = (%s, %s)", dec, reason)
	}
	if dec, reason := decideHost(Policy{Default: Deny}, "example.com"); dec != Deny || reason != "default" {
		t.Errorf("默认 deny = (%s, %s)", dec, reason)
	}
	// 零值 Policy 的 Default 是空串：按 deny 处理（安全侧），而不是当成 allow。
	if dec, _ := decideHost(Policy{}, "example.com"); dec != Deny {
		t.Error("零值策略必须落到 deny")
	}
}

func TestSetPolicyValidation(t *testing.T) {
	ctl := openTestProxy(t, Policy{Default: Deny})
	ctx := context.Background()

	bad := []Policy{
		{Default: "banana"},
		{Default: Deny, Rules: []Rule{{Pattern: ""}}},
		{Default: Deny, Rules: []Rule{{Pattern: "a.example", Decision: "maybe"}}},
	}
	for i, p := range bad {
		if err := ctl.SetPolicy(ctx, "t1", p); err == nil {
			t.Errorf("第 %d 个非法策略应被拒绝", i)
		}
	}
	if err := ctl.SetPolicy(ctx, "", Policy{Default: Allow}); err == nil {
		t.Error("空 tenantID 应被拒绝")
	}
}

func TestPolicyDefaultAndCloneIsolation(t *testing.T) {
	base := Policy{
		Default: Deny,
		Rules:   []Rule{{Pattern: "a.example", Decision: Allow, Note: "n"}},
	}
	ctl := openTestProxy(t, base)
	ctx := context.Background()

	got, err := ctl.Policy(ctx, "未配置的租户")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("未配置租户应返回默认策略: %+v", got)
	}
	// 修改返回值不能污染内部状态。
	got.Rules[0].Decision = Deny
	again, _ := ctl.Policy(ctx, "未配置的租户")
	if again.Rules[0].Decision != Allow {
		t.Error("Policy() 返回了内部切片，调用方修改后影响了存储")
	}
}

func TestPolicyPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	first := openProxyAt(t, dir, Policy{Default: Deny})

	want := Policy{
		Default:      Allow,
		AllowPrivate: true,
		LogBody:      false,
		Rules: []Rule{
			{Pattern: "*.corp.example", Decision: Deny, Note: "内部"},
			{Pattern: "api.corp.example", Decision: Ask},
		},
	}
	if err := first.SetPolicy(context.Background(), "t1", want); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openProxyAt(t, dir, Policy{Default: Deny})
	got, err := second.Policy(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("重启后策略 = %+v, want %+v", got, want)
	}
	other, _ := second.Policy(context.Background(), "t2")
	if other.Default != Deny {
		t.Errorf("未配置租户不该拿到 t1 的策略: %+v", other)
	}
}
