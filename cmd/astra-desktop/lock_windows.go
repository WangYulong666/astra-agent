//go:build windows

package main

import (
	"errors"
	"os"
)

// Windows 下用 OpenFile 独占打开近似单实例（ LockFileEx 更严谨，这里够用）。
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.New("另一个 Astra 实例正持有锁")
	}
	return func() { _ = f.Close() }, nil
}
