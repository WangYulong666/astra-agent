//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setpgid：独立进程组，取消时能连子进程一起杀（`sleep 100 | cat` 不留孤儿）。
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
