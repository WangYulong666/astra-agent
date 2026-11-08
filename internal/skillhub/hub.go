package skillhub

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
)

// 装配桩：由 T2 替换成真实现。
func openImpl(_ Config, _ *eventbus.Hub) (Hub, error) {
	return nil, fmt.Errorf("skillhub: 实现未接入")
}

var _ = context.Background
