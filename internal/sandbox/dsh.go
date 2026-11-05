package sandbox

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// dsh/astra-trim.yml 是 dsh 的 profile 覆盖层（--patch 用）。
//
// 内嵌进二进制而不是从仓库路径读：`bin/` 里的一组二进制拷到任何机器都该能跑，
// 不该依赖「deploy 目录恰好还在相对位置」。同时它也是版本的一部分——
// dsh 升级后裁剪项变了，这个文件必须跟着二进制一起换，而不是留着旧副本。
//
//go:embed dsh/astra-trim.yml
var dshFS embed.FS

// DSHTrimPath 把内嵌的裁剪层落盘，返回 --patch 需要的路径。
//
// 文件名带内容哈希：内容变了自然换文件，旧文件不会被误用；内容没变就复用，
// 不会每次启动都产生一个新的临时文件。返回 false 表示这个平台取不到可写目录，
// 调用方应当退化成「不带 patch 启动」（此时 dsh 可能因日志串台而握手失败，
// 所以会顺带打一条告警）。
func DSHTrimPath() (string, bool) {
	b, err := dshFS.ReadFile("dsh/astra-trim.yml")
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(b)
	name := fmt.Sprintf("astra-dsh-trim-%s.yml", hex.EncodeToString(sum[:6]))

	dir := filepath.Join(os.TempDir(), "astra-dsh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false
	}
	p := filepath.Join(dir, name)
	// 已存在就当作命中：内容由哈希决定，同名必同内容。
	if st, err := os.Stat(p); err == nil && st.Size() == int64(len(b)) {
		return p, true
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", false
	}
	return p, true
}
