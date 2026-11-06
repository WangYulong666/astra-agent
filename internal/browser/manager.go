package browser

import "fmt"

// Open 探测 Chrome 并返回管理器。
func Open(cfg Config) (Manager, error) {
	return nil, fmt.Errorf("browser: 实现未接入（chrome=%s）", cfg.ChromePath)
}
