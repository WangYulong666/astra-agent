package runner

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// coalesceLimit 单个缓冲区的上限。超过就立刻落库，避免长回答在内存里憋太久。
const coalesceLimit = 4 << 10

// sink 把 ACP 的 session/update 转成事件写入 store 并扇出。
//
// 为什么必须合并：agent 的文本增量是**逐 token** 的，一次长回答能有几千条。
// 逐条落库既浪费 fsync，也会让事件表被单个任务打爆（架构文档已写明这点）。
// 做法是按 50ms 窗口合并同类增量，块大小上限 4KB。
//
// 顺序保证：ACP 的通知在连接层是同步分发的（见 internal/acp/conn.go），
// 所以 update() 天然串行；这里再加锁只是为了和定时 flush 互斥。
type sink struct {
	ctx    context.Context
	st     store.Store
	hub    *eventbus.Hub
	runID  string
	window time.Duration

	mu     sync.Mutex
	order  []string // 按首次出现的顺序，保证 thought 与 message 的相对次序不被打乱
	pend   map[string]*strings.Builder
	timer  *time.Timer
	closed bool
	err    error
}

func newSink(ctx context.Context, st store.Store, hub *eventbus.Hub, runID string, window time.Duration) *sink {
	if window <= 0 {
		window = 50 * time.Millisecond
	}
	return &sink{ctx: ctx, st: st, hub: hub, runID: runID, window: window, pend: map[string]*strings.Builder{}}
}

// delta 追加一段增量文本，必要时触发合并落库。
func (s *sink) delta(typ, text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	b, ok := s.pend[typ]
	if !ok {
		b = &strings.Builder{}
		s.pend[typ] = b
		s.order = append(s.order, typ)
	}
	b.WriteString(text)

	size := 0
	for _, v := range s.pend {
		size += v.Len()
	}
	if size >= coalesceLimit {
		s.flushLocked()
		return
	}
	if s.timer == nil {
		s.timer = time.AfterFunc(s.window, func() {
			s.mu.Lock()
			s.flushLocked()
			s.mu.Unlock()
		})
	}
}

// event 写一条非增量事件。写之前先 flush，保证「文本 → 工具调用」的顺序不乱。
func (s *sink) event(typ string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.flushLocked()
	s.appendLocked(typ, payload)
}

// flush 把缓冲区里的增量全部落库。
func (s *sink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// close 收尾：flush 并停掉定时器。之后不再接受写入。
func (s *sink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	return s.err
}

func (s *sink) flushLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if len(s.order) == 0 {
		return
	}
	order := s.order
	pend := s.pend
	s.order = nil
	s.pend = map[string]*strings.Builder{}

	for _, typ := range order {
		b := pend[typ]
		if b == nil || b.Len() == 0 {
			continue
		}
		s.appendLocked(typ, map[string]any{"text": b.String()})
	}
}

func (s *sink) appendLocked(typ string, payload any) {
	if s.err != nil {
		return
	}
	ev, err := s.st.Append(s.ctx, s.runID, store.NewEvent{Type: typ, Payload: payload})
	if err != nil {
		s.err = err
		return
	}
	if s.hub != nil {
		s.hub.Publish(ev)
	}
}
