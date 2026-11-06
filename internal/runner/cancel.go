package runner

import "sync"

// cancelGate 解决「取消发得比 prompt 还早」这个竞态。
//
// 现象：用户看到界面转起来了就按停止，此时控制面已把事件推出去，但 prompt
// 请求可能还在路上。若直接把 session/cancel 发给 agent，agent 侧此时没有
// 进行中的 prompt，取消会被静默丢弃——然后那一轮照跑到结束，用户看到的是
// 「我按了停止，它还在跑，最后显示已完成」。
//
// 做法：把「用户要取消」和「prompt 已发出」两个状态放在同一把锁下，
// 让二者必有一个先看到对方：
//   - 取消先落 → beforePrompt 直接不发 prompt，本轮记为 cancelled
//   - prompt 先发 → requestCancel 返回 true，按正常路径发 ACP 取消
//
// 之所以不用两个原子变量：两条独立的写入没有任何顺序保证，
// 交叉执行时两边都会以为自己该沉默，取消就丢了。
type cancelGate struct {
	mu sync.Mutex
	m  map[string]*runGate
}

type runGate struct {
	dispatched bool // prompt 已经发给 agent
	cancel     bool // 用户请求过取消
}

func newCancelGate() *cancelGate { return &cancelGate{m: map[string]*runGate{}} }

func (g *cancelGate) get(runID string) *runGate {
	if g.m == nil {
		g.m = map[string]*runGate{}
	}
	s, ok := g.m[runID]
	if !ok {
		s = &runGate{}
		g.m[runID] = s
	}
	return s
}

// requestCancel 记录取消意图，并告诉调用方「要不要发 ACP 取消」。
// prompt 还没发出去时返回 false——那时候发取消只会被 agent 丢掉。
func (g *cancelGate) requestCancel(runID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(runID)
	s.cancel = true
	return s.dispatched
}

// beforePrompt 在把 prompt 交给 agent 之前调用。返回 false 表示这轮已经作废。
func (g *cancelGate) beforePrompt(runID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(runID)
	if s.cancel {
		return false
	}
	s.dispatched = true
	return true
}

// afterPrompt 在 prompt 返回后调用，返回这轮是否应记为「已取消」。
//
// 即便 agent 正常跑完，只要用户在它结束前按过停止，也记成 cancelled：
// 两个事件差几毫秒，对用户来说「我按了停止它停了」比「我按了停止它说成功」
// 更符合直觉，也和按钮的语义一致。
func (g *cancelGate) afterPrompt(runID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.get(runID)
	s.dispatched = false
	return s.cancel
}

// cancelled 报告这一轮是否已被请求取消。
//
// 审批回调需要它：结论是 deny 时通常要把 run 置回 running（agent 会换个工具继续），
// 但如果用户已经取消，置 running 就是错的——收尾马上会写 cancelled，
// 两边一撞就成了「终态被覆盖回 running」。
func (g *cancelGate) cancelled(runID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.get(runID).cancel
}

// forget 在本轮结束后清理，避免长跑进程里地图无限增长。
func (g *cancelGate) forget(runID string) {
	g.mu.Lock()
	delete(g.m, runID)
	g.mu.Unlock()
}
