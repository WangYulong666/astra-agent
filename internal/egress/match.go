package egress

import (
	"net"
	"strings"
)

// normalizeHost 把「用户可能写出来的主机串」收敛成可比较的形式：
// 去空白、去端口、去 IPv6 方括号、去末尾根点、转小写。
//
// 端口不参与策略匹配（见 Rule 的注释），所以这里必须剥掉；
// 客户端可能写成 `Example.COM:443`、`[::1]:8443`，都要归一。
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "[") {
		// [::1]:443 / [::1]
		if i := strings.IndexByte(s, ']'); i > 0 {
			s = s[1:i]
		}
	} else if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	} else if i := strings.LastIndexByte(s, ':'); i > 0 && strings.Count(s, ":") == 1 {
		// example.com:443 这种「像端口但不是合法 host:port」的写法
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

// matchPattern 判断 host 是否命中 pattern。规则：
//
//	`*`              全部
//	`*.example.com`  子域（不含 example.com 本身）
//	`*example.com`   后缀匹配（含 example.com 本身）
//	`api.example.com` 精确匹配
//
// 全部大小写不敏感，端口在两边都会被剥掉。
func matchPattern(pattern, host string) bool {
	pat := normalizeHost(pattern)
	h := normalizeHost(host)
	if pat == "" || h == "" {
		return false
	}
	switch {
	case pat == "*":
		return true
	case strings.HasPrefix(pat, "*."):
		suffix := pat[1:] // ".example.com"
		return strings.HasSuffix(h, suffix) && len(h) > len(suffix)
	case strings.HasPrefix(pat, "*"):
		return strings.HasSuffix(h, pat[1:])
	default:
		return h == pat
	}
}

// isPrivateIP 判断一个已解析的地址是否属于「不允许沙箱访问」的网段：
//
//	10/8、172.16/12、192.168/16  —— 内网
//	127/8、::1                   —— 本机（代理自身就跑在本机）
//	169.254/16、fe80::/10        —— 链路本地；169.254.169.254 是云元数据
//	fc00::/7                     —— IPv6 唯一本地地址
//
// 只看字面 IP；域名会由拨号器的 Control 在 DNS 解析之后拿到真实 IP 再判，
// 避免「先解析、后拨号」之间被 DNS rebinding 换掉。
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	return false
}

// splitTarget 把 host:port 拆开；没有端口时用默认端口补上。
// 端口必须是 1-65535 的数字——非法端口没必要浪费一次拨号。
func splitTarget(hostport, defaultPort string) (host, port string, err error) {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return "", "", errBadTarget
	}
	if h, p, e := net.SplitHostPort(hostport); e == nil {
		if h == "" || p == "" {
			return "", "", errBadTarget
		}
		return h, p, validatePort(p)
	}
	h := hostport
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", "", errBadTarget
	}
	return h, defaultPort, nil
}

func validatePort(p string) error {
	n := 0
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return errBadTarget
		}
		n = n*10 + int(p[i]-'0')
		if n > 65535 {
			return errBadTarget
		}
	}
	if n == 0 {
		return errBadTarget
	}
	return nil
}
