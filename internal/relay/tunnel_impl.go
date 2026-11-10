package relay

import "fmt"

// 装配桩：由 W1 替换成真实现。
func dialImpl(_ TunnelConfig) (Tunnel, error) {
	return nil, fmt.Errorf("relay: 隧道未接入")
}
