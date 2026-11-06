package distill

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// Open 装配沉淀器。st 用于读 run 与事件流。
func Open(cfg Config, st store.Store) (Distiller, error) {
	return nil, fmt.Errorf("distill: 实现未接入（dir=%s）", cfg.SkillDir)
}

// 编译期占位，确保接口被实现方引用（实现后删除）。
var _ = context.Background
