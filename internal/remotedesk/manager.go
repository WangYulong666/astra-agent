package remotedesk

import "fmt"

// 装配桩：由 R1 替换成真实现（配对码、设备、会话、审批、限速、审计）。
func openImpl(cfg Config) (Manager, error) {
	_ = cfg
	return nil, fmt.Errorf("remotedesk: 实现未接入")
}
