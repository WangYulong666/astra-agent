package egress

import (
	"fmt"
	"sync"
	"testing"
)

func TestRingKeepsOnlyLatest(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		r.add(LogEntry{Host: fmt.Sprintf("h%d", i), TenantID: "t"})
	}
	if got := r.len(); got != 3 {
		t.Fatalf("环形缓冲条数 = %d, want 3", got)
	}
	got := r.list("", 0)
	if len(got) != 3 {
		t.Fatalf("list 条数 = %d, want 3", len(got))
	}
	want := []string{"h4", "h3", "h2"} // 倒序：最新的在最前
	for i, w := range want {
		if got[i].Host != w {
			t.Errorf("list[%d].Host = %q, want %q", i, got[i].Host, w)
		}
	}
}

func TestRingOrderLimitAndTenant(t *testing.T) {
	r := newRing(10)
	for i := 0; i < 6; i++ {
		tenant := "a"
		if i%2 == 1 {
			tenant = "b"
		}
		r.add(LogEntry{Host: fmt.Sprintf("h%d", i), TenantID: tenant})
	}
	all := r.list("", 0)
	if len(all) != 6 || all[0].Host != "h5" || all[5].Host != "h0" {
		t.Fatalf("全量倒序不对: %+v", all)
	}
	if limited := r.list("", 2); len(limited) != 2 || limited[0].Host != "h5" {
		t.Fatalf("limit 未生效: %+v", limited)
	}
	onlyA := r.list("a", 0)
	if len(onlyA) != 3 {
		t.Fatalf("租户过滤条数 = %d, want 3", len(onlyA))
	}
	for _, e := range onlyA {
		if e.TenantID != "a" {
			t.Errorf("租户过滤漏了 %q", e.TenantID)
		}
	}
	if none := r.list("nobody", 0); len(none) != 0 {
		t.Errorf("不存在的租户应返回空，得到 %d 条", len(none))
	}
}

func TestRingUpdateAfterEviction(t *testing.T) {
	r := newRing(1)
	a := r.add(LogEntry{Host: "a"})
	_ = r.add(LogEntry{Host: "b"})
	// a 已被覆盖；回填写到指针上不应该 panic，也不应该把 a 塞回缓冲。
	r.update(a, func(e *LogEntry) { e.BytesUp = 42 })
	if got := r.list("", 0); len(got) != 1 || got[0].Host != "b" {
		t.Fatalf("被覆盖的记录不应复活: %+v", got)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := newRing(128)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				e := r.add(LogEntry{Host: fmt.Sprintf("g%d-%d", g, i), TenantID: "t"})
				r.update(e, func(x *LogEntry) {
					x.BytesUp = int64(i)
					x.BytesDown = int64(i * 2)
				})
			}
		}(g)
	}
	// 读侧同时跑，验证读写锁没有漏。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = r.list("t", 10)
		}
	}()
	wg.Wait()
	if got := r.len(); got != 128 {
		t.Fatalf("并发写入后条数 = %d, want 128", got)
	}
}
