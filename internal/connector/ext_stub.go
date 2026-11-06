package connector

import "context"

// 装配桩：由「连接器 proxy 注入」任务替换成真实现。
// 放在独立文件是为了让主控的骨架先编译通过，实现方直接覆盖本文件即可。
func (m *manager) TokenFor(ctx context.Context, tenantID, userID, name string) (string, error) {
	return "", ErrInjectProxyUnimplemented
}

func (m *manager) ConnectorForHost(host string) (Connector, bool) {
	return Connector{}, false
}
