package relay

import (
	"net"
	"sync"
)

// watchedConn 是「底层连接死掉」的探测器。
//
// 为什么需要它：smux v1.5.59 的 recvLoop 读到错误时只 close 内部 channel
// （chSocketReadError），**不会关闭 Session**，所以 Session.CloseChan() 不会触发——
// 如果只监听 CloseChan，对端拔网线/进程被杀之后，注册表要等到 keepalive 超时
// （默认 90s）才会摘除设备，期间所有请求都会打到一条死隧道上。
//
// 做法：把 net.Conn 包一层，Read/Write 出现任何错误就关闭 dead 通道；
// 上层（relay 的设备注册表、tunnel 的重连逻辑）监听 dead 立即动作。
type watchedConn struct {
	net.Conn
	dead chan struct{}
	once sync.Once
}

func watchConn(c net.Conn) *watchedConn {
	return &watchedConn{Conn: c, dead: make(chan struct{})}
}

func (w *watchedConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if err != nil {
		w.markDead()
	}
	return n, err
}

func (w *watchedConn) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err != nil {
		w.markDead()
	}
	return n, err
}

func (w *watchedConn) markDead() {
	w.once.Do(func() { close(w.dead) })
}

// Dead 在底层连接读写失败后关闭。
func (w *watchedConn) Dead() <-chan struct{} { return w.dead }
