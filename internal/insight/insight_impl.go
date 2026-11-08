package insight

import (
	"log"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// insight.go 冻结了 Open / Attach 的签名，这里只做转发：
//   - 文件存储实现见 store.go
//   - 终态订阅与规则式诊断实现见 attach.go

func openImpl(dataDir string) (Store, error) {
	return sharedStore(dataDir)
}

func attachImpl(hub *eventbus.Hub, st store.Store, act Activity, dataDir string, lg *log.Logger) func() {
	return newAttacher(hub, st, act, dataDir, lg)
}
