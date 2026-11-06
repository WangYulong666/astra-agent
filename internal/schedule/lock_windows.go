//go:build windows

package schedule

// Windows 没有 flock，标准库的 syscall 也没暴露 LockFileEx 的易用封装。
//
// 这里退化成「不加跨进程锁」：进程内的 mutex 仍然生效，
// 而且写是「临时文件 + rename」，所以文件永远不会出现半个 JSON；
// 代价是两个进程同时写时可能丢更新（后写的覆盖先写的）。
// 桌面端单实例运行，这个取舍可以接受；多副本部署在 Linux 上，走的是 flock。
func flockFile(fd uintptr, exclusive bool) error { return nil }

func funlockFile(fd uintptr) error { return nil }
