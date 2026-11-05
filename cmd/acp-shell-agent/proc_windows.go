//go:build windows

package main

import (
	"errors"
	"os/exec"
)

// Windows 没有 POSIX 进程组；shell agent 的取消语义在 Windows 上不完整。
// stub 只为交叉编译通过。
func setPgid(*exec.Cmd) {}

func killTree(int) error { return errors.New("windows: 不支持进程组终止") }
