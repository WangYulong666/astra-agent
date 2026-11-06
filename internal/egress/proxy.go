package egress

import "fmt"

// Open 启动出网代理。实现见 proxy_impl 待填。
func Open(cfg Config) (Controller, error) {
	return nil, fmt.Errorf("egress: 代理实现未接入（LISTEN=%s）", cfg.ListenAddr)
}
