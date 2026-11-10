package main

import (
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestParseOptionsDefaults(t *testing.T) {
	o, err := parseOptions(nil, noEnv)
	if err != nil {
		t.Fatalf("默认参数应通过: %v", err)
	}
	if o.addr != ":9443" {
		t.Fatalf("默认监听地址 = %q, 期望 :9443", o.addr)
	}
	if o.maxStreams != 128 {
		t.Fatalf("默认并发流上限 = %d, 期望 128", o.maxStreams)
	}
	if o.idleTimeout != 90*time.Second {
		t.Fatalf("默认空闲超时 = %v, 期望 90s", o.idleTimeout)
	}
	if o.tlsEnabled() {
		t.Fatal("默认不应启用 TLS")
	}
	if !o.debug() {
		t.Fatal("没给 token 应判定为调试模式")
	}
}

func TestParseOptionsTokenFromEnv(t *testing.T) {
	o, err := parseOptions(nil, env(map[string]string{"ASTRA_RELAY_TOKEN": " env-token "}))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if o.token != "env-token" {
		t.Fatalf("token = %q, 期望 env-token（应去掉空白）", o.token)
	}
	if o.debug() {
		t.Fatal("有 token 时不应是调试模式")
	}
}

func TestParseOptionsFlagBeatsEnv(t *testing.T) {
	o, err := parseOptions([]string{"-token", "flag-token"}, env(map[string]string{"ASTRA_RELAY_TOKEN": "env-token"}))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if o.token != "flag-token" {
		t.Fatalf("token = %q, 期望 flag-token", o.token)
	}
}

func TestParseOptionsTLSPairing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // 期望错误里的关键词，空表示应成功
		tls  bool
	}{
		{"只有证书", []string{"-tls-cert", "/tmp/c.pem"}, "成对", false},
		{"只有私钥", []string{"-tls-key", "/tmp/k.pem"}, "成对", false},
		{"两个都给", []string{"-tls-cert", "/tmp/c.pem", "-tls-key", "/tmp/k.pem"}, "", true},
		{"都不给", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseOptions(tc.args, noEnv)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("期望错误含 %q，实际 err=%v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应成功: %v", err)
			}
			if o.tlsEnabled() != tc.tls {
				t.Fatalf("tlsEnabled = %v, 期望 %v", o.tlsEnabled(), tc.tls)
			}
		})
	}
}

func TestParseOptionsRejects(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"空地址", []string{"-addr", ""}, "addr"},
		{"零并发", []string{"-max-streams", "0"}, "max-streams"},
		{"负并发", []string{"-max-streams", "-3"}, "max-streams"},
		{"空闲超时太小", []string{"-idle-timeout", "1s"}, "idle-timeout"},
		{"多余参数", []string{"extra"}, "不认识的参数"},
		{"未知 flag", []string{"-nope"}, "flag"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseOptions(tc.args, noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误含 %q，实际 err=%v", tc.want, err)
			}
		})
	}
}

func TestParseOptionsCustom(t *testing.T) {
	o, err := parseOptions([]string{
		"-addr", "127.0.0.1:0",
		"-public-url", "https://relay.example.com",
		"-max-streams", "16",
		"-idle-timeout", "45s",
	}, noEnv)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if o.addr != "127.0.0.1:0" || o.publicURL != "https://relay.example.com" ||
		o.maxStreams != 16 || o.idleTimeout != 45*time.Second {
		t.Fatalf("解析结果不符: %+v", o)
	}
}

// banner 必须把「调试模式」和「无 token 谁都能注册设备」说清楚。
func TestBannerWarnsWithoutToken(t *testing.T) {
	var sb strings.Builder
	banner(&sb, options{addr: ":9443", maxStreams: 128, idleTimeout: 90 * time.Second}, "0.0.0.0:9443")
	out := sb.String()
	for _, want := range []string{"监听地址", "TLS", "调试模式", "危险", "token"} {
		if !strings.Contains(out, want) {
			t.Fatalf("banner 缺少 %q:\n%s", want, out)
		}
	}
}

func TestBannerShowsPublicURL(t *testing.T) {
	var sb strings.Builder
	banner(&sb, options{
		addr: ":443", publicURL: "https://relay.example.com", token: "x",
		tlsCert: "c", tlsKey: "k", maxStreams: 64, idleTimeout: time.Minute,
	}, "0.0.0.0:443")
	out := sb.String()
	if strings.Contains(out, "危险") {
		t.Fatalf("有 token 时不该出现调试告警:\n%s", out)
	}
	if !strings.Contains(out, "https://relay.example.com/t/<device>/...") {
		t.Fatalf("banner 应给出手机访问地址:\n%s", out)
	}
	if !strings.Contains(out, "wss://relay.example.com") {
		t.Fatalf("banner 应给出设备端 ASTRA_RELAY_URL:\n%s", out)
	}
}
