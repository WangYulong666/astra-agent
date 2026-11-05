// Package eventbus 是事件的内存扇出。
//
// 存储负责「事件不丢」，这里只负责「实时推给正在看的连接」。
// 拆成两层的原因：多副本阶段扇出会换成 PG LISTEN/NOTIFY，而存储语义不变；
// 反过来，SSE 连接的生命周期也不该由存储层管。
package eventbus

import (
	"sync"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// DefaultBuffer 单连接缓冲条数。与架构文档一致：超了主动断开让客户端带
// after_seq 重连续传，而不是无限堆积把进程内存吃光。
const DefaultBuffer = 512

// Hub 维护 runID → 订阅者集合。
type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[*Sub]struct{}
	// globalFns 是进程内全局消费者（桌面通知、审计旁路、观测、记忆入库）。
	//
	// 早期这里只放了一个函数，后注册的会覆盖先注册的——加了第二个消费者
	// （trace 与 memory 都要订阅 run 终态）之后，谁后装配谁就把别人顶掉，
	// 症状是「观测和记忆只能活一个」，而且没有任何报错。
	// 现在改成列表：追加语义，Publish 时按注册顺序逐个调。
	globalFns []func(store.Event)
}

func New() *Hub { return &Hub{subs: map[string]map[*Sub]struct{}{}} }

// Subscribe 订阅某个 run 的后续事件。afterSeq 之后的历史由调用方从 store 补齐，
// 这里是「从现在开始」，避免补齐与实时之间出现空洞（见 api 层的做法）。
func (h *Hub) Subscribe(runID string) *Sub {
	s := &Sub{hub: h, runID: runID, ch: make(chan store.Event, DefaultBuffer)}

	h.mu.Lock()
	if h.subs[runID] == nil {
		h.subs[runID] = map[*Sub]struct{}{}
	}
	h.subs[runID][s] = struct{}{}
	h.mu.Unlock()
	return s
}

// OnGlobal 注册全局事件回调（可注册多个，追加语义）。
//
// 回调在 Publish 调用方的 goroutine 里同步执行，必须快、不许阻塞、不许 panic——
// 它挡着所有 SSE 推送的路。重活要自己丢进队列（trace/memory 都是这么做的）。
func (h *Hub) OnGlobal(fn func(store.Event)) {
	if fn == nil {
		return
	}
	h.mu.Lock()
	h.globalFns = append(h.globalFns, fn)
	h.mu.Unlock()
}

// Publish 把事件推给所有订阅者。不阻塞：缓冲满就关掉那个订阅，
// 由客户端凭 Last-Event-ID / after_seq 重连补齐——丢的是通知，不是数据。
func (h *Hub) Publish(ev store.Event) {
	h.mu.RLock()
	fns := make([]func(store.Event), len(h.globalFns))
	copy(fns, h.globalFns)
	for s := range h.subs[ev.RunID] {
		s.push(ev)
	}
	h.mu.RUnlock()
	// 在锁外回调：消费者如果再碰 Hub（例如订阅）不能死锁。
	// panic 单独隔离：一个消费者崩了不能连累 SSE 扇出和其他消费者。
	for _, fn := range fns {
		func() {
			defer func() { _ = recover() }()
			fn(ev)
		}()
	}
}

func (h *Hub) remove(s *Sub) {
	h.mu.Lock()
	if set, ok := h.subs[s.runID]; ok {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.runID)
		}
	}
	h.mu.Unlock()
}

// Sub 是单个订阅者。
type Sub struct {
	hub   *Hub
	runID string
	ch    chan store.Event

	mu         sync.Mutex
	closed     bool
	overflowed bool
}

// Events 返回事件通道。通道被关闭表示「需要重连续传」。
func (s *Sub) Events() <-chan store.Event { return s.ch }

// Overflowed 表示是因为消费太慢被主动断开的。
func (s *Sub) Overflowed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overflowed
}

func (s *Sub) push(ev store.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.overflowed = true
		s.closed = true
		close(s.ch)
		s.hub.remove(s)
	}
}

// Close 取消订阅。可重复调用。
func (s *Sub) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	s.hub.remove(s)
}
