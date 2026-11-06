package egress

import (
	"net"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"Example.COM":         "example.com",
		"example.com:8080":    "example.com",
		"example.com.":        "example.com",
		"  api.example.com  ": "api.example.com",
		"[::1]:443":           "::1",
		"[::1]":               "::1",
		"::1":                 "::1",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		// 精确
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "API.EXAMPLE.COM", true},
		{"api.example.com", "api.example.com:443", true},
		{"api.example.com:443", "api.example.com", true},
		{"api.example.com", "x.api.example.com", false},
		{"api.example.com", "api.example.org", false},
		// 通配（*. 不含裸域）
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "notexample.com", false},
		{"*.example.com", "A.Example.Com:8443", true},
		// 无点的 * 前缀：后缀匹配，含裸域
		{"*example.com", "example.com", true},
		{"*example.com", "api.example.com", true},
		{"*example.com", "example.org", false},
		// 全部
		{"*", "anything.at.all", true},
		{"*", "", false},
		// 空
		{"", "example.com", false},
		{"example.com", "", false},
	}
	for _, c := range cases {
		if got := matchPattern(c.pattern, c.host); got != c.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", c.pattern, c.host, got, c.want)
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"11.0.0.1", false},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"172.15.255.255", false},
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"192.169.0.1", false},
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		{"169.254.169.254", true}, // 云元数据，必须拦
		{"169.254.0.1", true},
		{"169.255.0.1", false},
		{"8.8.8.8", false},
		{"::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"2001:4860:4860::8888", false},
		{"::ffff:127.0.0.1", true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("测试数据里的 IP 不合法: %s", c.ip)
		}
		if got := isPrivateIP(ip); got != c.want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
	if isPrivateIP(nil) {
		t.Error("isPrivateIP(nil) 应为 false")
	}
}
