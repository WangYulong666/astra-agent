package acp

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// StdioConn 把一个 agent 子进程的 stdin/stdout 包成 io.ReadWriteCloser，
// 使 Conn 可以在本地直接驱动 agent，不需要沙箱和网络。
//
// 关键约束：**stderr 必须有人读**。agent 把日志写 stderr（ACP 要求），
// 如果这边不读，管道缓冲区写满（约 64KB）后 agent 会阻塞，表现为「agent 卡住不说话」。
type StdioConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	stderrDone chan struct{}

	closeOnce sync.Once
}

// StdioOption 配置 StdioConn。
type StdioOption func(*stdioConfig)

type stdioConfig struct {
	stderr io.Writer
	env    []string
	dir    string
}

// WithStderr 指定 agent 的 stderr 去向。默认 os.Stderr。
// 传 io.Discard 可以静默（不推荐——ACP 问题基本都是靠 stderr 看出来的）。
func WithStderr(w io.Writer) StdioOption {
	return func(c *stdioConfig) { c.stderr = w }
}

// WithEnv 追加环境变量（"K=V" 形式）。
func WithEnv(env ...string) StdioOption {
	return func(c *stdioConfig) { c.env = append(c.env, env...) }
}

// WithDir 指定工作目录。
func WithDir(dir string) StdioOption {
	return func(c *stdioConfig) { c.dir = dir }
}

// StartStdio 启动命令并返回包装好的连接。
// 返回前子进程已经 Start 成功，所以调用方可以立刻开始发 initialize。
func StartStdio(name string, args []string, opts ...StdioOption) (*StdioConn, error) {
	cfg := stdioConfig{stderr: os.Stderr}
	for _, o := range opts {
		o(&cfg)
	}

	cmd := exec.Command(name, args...)
	if cfg.dir != "" {
		cmd.Dir = cfg.dir
	}
	if len(cfg.env) > 0 {
		cmd.Env = append(os.Environ(), cfg.env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	s := &StdioConn{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderrDone: make(chan struct{}),
	}

	// 持续把 stderr 排空，避免管道写满把 agent 堵死
	go func() {
		defer close(s.stderrDone)
		_, _ = io.Copy(cfg.stderr, stderrPipe)
	}()

	return s, nil
}

func (s *StdioConn) Read(p []byte) (int, error)  { return s.stdout.Read(p) }
func (s *StdioConn) Write(p []byte) (int, error) { return s.stdin.Write(p) }

// Close 关 stdin 让 agent 自行退出；超时后强杀。
// 顺序很重要：先关 stdin 发 EOF，agent 有机会 flush 并优雅退出。
func (s *StdioConn) Close() error {
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()

		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-done
		}
		<-s.stderrDone
	})
	return nil
}

// PID 返回子进程号，便于日志定位。
func (s *StdioConn) PID() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Stdio 把当前进程的 stdin/stdout 包成 io.ReadWriteCloser，供 agent 侧使用。
//
// 约束和客户端侧一样：**stdout 只能走协议帧**。日志必须写 stderr，
// 否则往 stdout 打一行日志就会污染 NDJSON 流，客户端会报 JSON 解析错误。
func Stdio() io.ReadWriteCloser { return stdioPair{} }

type stdioPair struct{}

func (stdioPair) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdioPair) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdioPair) Close() error                { return nil }
