package capability

import (
	"context"
	"fmt"
)

// 装配桩：由 T1 替换成真实现。
func openImpl(_ Sources) (Registry, error) {
	return nil, fmt.Errorf("capability: 实现未接入")
}

var _ = context.Background
