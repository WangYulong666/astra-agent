package relay

import "fmt"

// 装配桩：由 W1 替换成真实现。
func openServerImpl(_ ServerConfig) (Server, error) {
	return nil, fmt.Errorf("relay: 服务端未接入")
}
