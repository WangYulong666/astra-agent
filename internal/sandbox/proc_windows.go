//go:build windows

package sandbox

import (
	"errors"
	"os/exec"
)

// local provider 在 Windows 上不可用（依赖 sandbox-exec / bwrap）。
// 这些 stub 只为交叉编译通过；真跑起来会在创建沙箱前被 provider 检查挡住。

func detach(*exec.Cmd) {}

func pidAlive(int) bool { return false }

func killGroup(int) {}

func terminate(int) error { return errors.New("sandbox: Windows 不支持 local provider") }

func killForce(int) {}
