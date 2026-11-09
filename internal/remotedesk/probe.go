package remotedesk

import (
	"strings"
	"sync"
	"time"
)

// 能力探测的公共部分：TTL 缓存、错误裁剪、给用户看的修复提示。
//
// 探测一律**只读**：不许用真实的点击/移动/按键来探测（那会改屏幕状态，
// 用户会看到鼠标自己乱跑）。抓屏可以真抓一帧，因为截图本来就是只读的。

const (
	// probeCacheTTL 是能力探测结果的缓存时长。TCC（辅助功能/屏幕录制）是
	// 可以在系统设置里随时改的，缓存太久用户就得重启服务才能生效。
	probeCacheTTL = 30 * time.Second
	// probeTimeout 是单次探测的上限，避免探测把 /capabilities 接口拖住。
	probeTimeout = 8 * time.Second
	// reasonMaxRunes 是 Reason 字段的最大长度（stderr 常常带一大堆废话）。
	reasonMaxRunes = 200
)

const (
	hintScreenRecording = "系统设置 → 隐私与安全性 → 屏幕与系统音频录制，勾选运行本服务的终端/程序后重启它"
	hintAccessibility   = "系统设置 → 隐私与安全性 → 辅助功能，勾选运行本服务的终端/程序后重启它"
	hintInstallCliclick = "brew install cliclick 可获得完整的鼠标控制（移动/右键/拖拽）"
)

// capabilityCache 是一个带 TTL 的 Capability 缓存。
func (c *capabilityCache) get(probe func() Capability) Capability {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		c.ttl = probeCacheTTL
	}
	if c.valid && time.Since(c.at) < c.ttl {
		return c.value
	}
	c.value = probe()
	c.at = time.Now()
	c.valid = true
	return c.value
}

func (c *capabilityCache) reset() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// capabilityCache 零值可用（ttl 零值取 probeCacheTTL）。
type capabilityCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	at    time.Time
	value Capability
	valid bool
}

// clipReason 把外部命令的 stderr 裁成一句话，塞进 Capability.Reason。
// 按 rune 裁，避免把中文截成半个字。
func clipReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > reasonMaxRunes {
		s = string(r[:reasonMaxRunes]) + "…"
	}
	return s
}

// isPermissionErr 判断 osascript / TCC 的权限类错误。
// -10004 权限违例；-1712 超时（System Events 卡住，多半也是权限没给）；
// not allowed / 不允许 是不同 macOS 版本的不同措辞。
func isPermissionErr(s string) bool {
	for _, m := range []string{"-10004", "-1712", "not allowed", "不允许", "权限违例", "not authorized", "Operation not permitted"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
