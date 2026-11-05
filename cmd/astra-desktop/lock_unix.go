//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// acquireFileLock 用 flock 保证单实例。f 故意不 Close：锁随进程存活。
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("另一个 Astra 实例正持有锁")
	}
	return func() { _ = f.Close() }, nil
}
