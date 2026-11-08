package runner

import (
	"strings"
	"testing"
)

func TestMergeBypassDedup(t *testing.T) {
	cases := []struct {
		name                  string
		existing, extra, want string
	}{
		{"只有控制面", "", "127.0.0.1:7788", "127.0.0.1:7788"},
		{"网关+控制面", "127.0.0.1:9000", "127.0.0.1:7788", "127.0.0.1:9000,127.0.0.1:7788"},
		{"extra 已存在去重", "127.0.0.1:7788,localhost:1", "127.0.0.1:7788", "127.0.0.1:7788,localhost:1"},
		{"existing 内部重复+空白", "a:1,a:1, b:2 ,", "c:3", "a:1,b:2,c:3"},
		{"全空", " , ,", "", ""},
		{"extra 空", "a:1,b:2", "", "a:1,b:2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mergeBypass(c.existing, c.extra); got != c.want {
				t.Fatalf("mergeBypass(%q,%q) = %q, want %q", c.existing, c.extra, got, c.want)
			}
		})
	}
}

// 控制面 host:port 必须「无条件」并入 NO_PROXY：与 NetworkLock 解耦。
// 网络锁开/关两种情况下都应放行控制面，并保留原有 LLM 网关、去重。
func TestEgressBypassValueUnconditionallyIncludesControlPlane(t *testing.T) {
	hints := NewRuntimeHints()
	hints.Set(nil, "http://127.0.0.1:7788", "")
	for _, lock := range []bool{false, true} {
		m := &Manager{cfg: Config{
			Hints:        hints,
			EgressBypass: "127.0.0.1:9000",
			NetworkLock:  lock,
		}}
		got := m.egressBypassValue()
		if !strings.Contains(got, "127.0.0.1:7788") {
			t.Fatalf("NetworkLock=%v 时 NO_PROXY 缺控制面: %q", lock, got)
		}
		if !strings.Contains(got, "127.0.0.1:9000") {
			t.Fatalf("NetworkLock=%v 时丢了原有 LLM 网关: %q", lock, got)
		}
		if strings.Count(got, "127.0.0.1:7788") != 1 {
			t.Fatalf("NetworkLock=%v 时控制面未去重: %q", lock, got)
		}
	}
}

// 与 NetworkLock 完全解耦：锁开和锁关拿到的 NO_PROXY 必须一致。
func TestEgressBypassValueSameWithAndWithoutLock(t *testing.T) {
	makeVal := func(lock bool) string {
		hints := NewRuntimeHints()
		hints.Set(nil, "http://127.0.0.1:7788", "")
		return (&Manager{cfg: Config{
			Hints: hints, EgressBypass: "10.0.0.1:3128", NetworkLock: lock,
		}}).egressBypassValue()
	}
	if makeVal(false) != makeVal(true) {
		t.Fatal("NetworkLock 影响了 NO_PROXY（应解耦）")
	}
}

// Start 之前 hints 的 url 为空：不放行任何控制面地址，也不产生多余逗号。
func TestEgressBypassValueBeforeStart(t *testing.T) {
	hints := NewRuntimeHints()
	hints.Set(nil, "", "")
	m := &Manager{cfg: Config{Hints: hints, EgressBypass: "127.0.0.1:9000"}}
	if got := m.egressBypassValue(); got != "127.0.0.1:9000" {
		t.Fatalf("url 为空时不应并入控制面, got %q", got)
	}
}

// EgressBypass 已含控制面时不重复追加。
func TestEgressBypassValueNoDuplicateControlPlane(t *testing.T) {
	hints := NewRuntimeHints()
	hints.Set(nil, "http://127.0.0.1:7788", "")
	m := &Manager{cfg: Config{Hints: hints, EgressBypass: "127.0.0.1:7788,127.0.0.1:7788"}}
	if got := m.egressBypassValue(); got != "127.0.0.1:7788" {
		t.Fatalf("控制面被重复追加: %q", got)
	}
}
