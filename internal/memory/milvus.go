// openMilvus 由 Milvus 实现补全。骨架阶段先返回明确错误，
// 保证「没配 Milvus 时平台照常启动」（Open 在 Addr 为空时走 Disabled）。
package memory

import "fmt"

func openMilvus(cfg Config) (Store, error) {
	return nil, fmt.Errorf("memory: Milvus 实现未接入（ADDR=%s）", cfg.Addr)
}
