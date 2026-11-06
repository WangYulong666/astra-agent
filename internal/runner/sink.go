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

// stage 落库一条事件但**先不发布**，返回事件供调用方在元数据写完之后 publish。
//
// 为什么需要拆开：终态要同时满足两类客户端——
//   - 轮询 run.Status 的客户端（前端首屏 / CLI / history 接口）看到终态就拉
//     事件流，此时终态事件必须已经在库里，否则最后一条是 message_delta；
//   - 订阅 SSE 的客户端收到「结束」立刻查 run，此时 run 必须已经是终态，
//     否则读到 running，事件与元数据自相矛盾。
//
// 把「落库」和「发布」绑成一步，无论先写谁都会有一类客户端看到中间态。
// 用法见 Manager.finish：stage 终态事件 → UpdateRun 终态 → publish。
func (s *sink) stage(typ string, payload any) store.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.Event{}
	}
	s.flushLocked()
	if s.err != nil {
		return store.Event{}
	}
	ev, err := s.st.Append(s.ctx, s.runID, store.NewEvent{Type: typ, Payload: payload})
	if err != nil {
		s.err = err
		return store.Event{}
	}
	return ev
}

// publish 发布一条由 stage 落好库的事件。零值事件（落库失败）直接忽略。
func (s *sink) publish(ev store.Event) {
	if s.hub != nil && ev.RunID != "" {
		s.hub.Publish(ev)
	}
}

func (s *sink) appendLocked(typ string, payload any) {
	ev := s.appendLockedStaged(typ, payload)
	s.publish(ev)
}

func (s *sink) appendLockedStaged(typ string, payload any) store.Event {
	if s.err != nil {
		return store.Event{}
	}
	ev, err := s.st.Append(s.ctx, s.runID, store.NewEvent{Type: typ, Payload: payload})
	if err != nil {
		s.err = err
		return store.Event{}
	}
	return ev
}
