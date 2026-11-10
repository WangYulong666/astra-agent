//go:build !darwin || !cgo

package remotedesk

// 没有加速抓屏后端的平台/构建：返回 (nil, nil) 表示「没有更快的选择」，
// defaultCapture 于是直接走平台实现（screencapture / gnome-screenshot / ...）。
//
// 这个文件的存在让 CGO_ENABLED=0 与 GOOS=linux 的构建完全看不到 cgo 代码。
func newAcceleratedCapture(cfg Config) (Capture, error) {
	_ = cfg
	return nil, nil
}
