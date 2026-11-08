package insight

import (
	"context"
	"fmt"
	"log"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// 装配桩：由 T3 替换成真实现。
func openImpl(string) (Store, error) { return nil, fmt.Errorf("insight: 实现未接入") }

func attachImpl(_ *eventbus.Hub, _ store.Store, _ Activity, _ string, _ *log.Logger) func() {
	return func() {}
}

var _ = context.Background
