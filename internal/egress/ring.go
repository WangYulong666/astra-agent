package egress

import "sync"

// defaultLogSize 是环形缓冲的默认条数。5000 条 × 约 200 字节 ≈ 1MB，
// 够覆盖「最近几次跑批」，又不至于让控制面吃内存。
const defaultLogSize = 5000

// ring 是固定容量的审计环形缓冲。
//
// 为什么不是 slice + trim：出网是高频动作，append 后 copy 会把每条日志
// 都变成 O(n)。环形缓冲写入永远是 O(1)，且天然「只留最近 N 条」。
//
// 入参和返回值都是值类型，但内部存指针：CONNECT 隧道要等连接结束才知道
// 字节数，届时回填同一条记录，而不是再补一条「补充说明」。
type ring struct {
	mu    sync.Mutex
	buf   []*LogEntry
	start int // 最旧一条的下标
	n     int // 当前条数
}

func newRing(size int) *ring {
	if size <= 0 {
		size = defaultLogSize
	}
	return &ring{buf: make([]*LogEntry, size)}
}

// add 写入一条记录并返回它的指针，供后续回填字节数。
func (r *ring) add(e LogEntry) *LogEntry {
	p := &e
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == len(r.buf) {
		// 满了：覆盖最旧的一条，并把起点前移
		r.buf[r.start] = p
		r.start = (r.start + 1) % len(r.buf)
		return p
	}
	r.buf[(r.start+r.n)%len(r.buf)] = p
	r.n++
	return p
}

// update 在锁内修改记录，避免和 Log() 的拷贝并发。
func (r *ring) update(e *LogEntry, fn func(*LogEntry)) {
	if e == nil {
		return
	}
	r.mu.Lock()
	fn(e)
	r.mu.Unlock()
}

// list 按时间倒序返回；tenant 非空时只返回该租户（"unowned" 也算一个「租户」）。
// limit <= 0 表示不限制。
func (r *ring) list(tenant string, limit int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, 0, min(r.n, max(limit, 0)))
	for k := 0; k < r.n; k++ {
		idx := (r.start + r.n - 1 - k) % len(r.buf)
		e := r.buf[idx]
		if e == nil {
			continue
		}
		if tenant != "" && e.TenantID != tenant {
			continue
		}
		out = append(out, *e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (r *ring) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}
