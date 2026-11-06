//go:build !windows

package schedule

import "syscall"

// flockFile 用 flock(2) 做跨进程锁。exclusive=false 是共享锁（读），true 是排他锁（写）。
//
// 带 LOCK_NB：拿不到立刻返回，由调用方重试。理由见 store_file.go 的 acquire。
func flockFile(fd uintptr, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	return syscall.Flock(int(fd), how|syscall.LOCK_NB)
}

func funlockFile(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}
