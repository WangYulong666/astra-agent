//go:build !windows

// 进程级原语的 Unix 实现，从 local.go 抽出来做平台隔离。
// local provider 依赖 sandbox-exec（macOS）/ bwrap（Linux），本来就不支持
// Windows；隔离只为让交叉编译通过、CI 的 GOOS=windows 检查不红。
package sandbox

import (
	"os/exec"
	"syscall"
)

// detach 让子进程脱离当前进程组：宿主退出后沙箱仍活着，可被 Acquire 认回。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// pidAlive 用 signal 0 做存在性检查，不真的发信号。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// killGroup 负 PID 杀整个进程组（dsh 是 acp-proxy 的子进程，单杀父会留孤儿）。
func killGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// terminate 先礼后兵的第一步。
func terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killForce 强杀单进程。
func killForce(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
