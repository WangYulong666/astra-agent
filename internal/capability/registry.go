package capability

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound 是 Get 精确匹配不到名字时返回的错误。调用方用
// errors.Is(err, capability.ErrNotFound) 判断，而不是比对字符串。
var ErrNotFound = errors.New("capability: not found")

// registry 把 Sources 的一次 Collect 结果缓存成不可变快照。
//
// 快照语义：Collect 只在 Open / Refresh 里调用；List / Get 只读缓存，
// 不触碰 Sources（Sources 可能查环境变量、扫目录，不能挂在读路径上）。
// items 每次刷新都整体替换，从不原地修改，因此读路径可以只持 RLock
// 拷出切片头，之后脱离锁遍历也是安全的。
type registry struct {
	src Sources

	// mu 保护 items 指针的替换。
	mu    sync.RWMutex
	items []Item

	// refreshMu 串行化 Collect：即使 Sources 本身不是并发安全的，
	// 也不会有两个 Collect 同时跑，更不会让旧快照被并发写覆盖。
	refreshMu sync.Mutex
}

// openImpl 装配注册表：至少成功 Collect 一次才算打开。
// src == nil 或首次 Collect 失败都返回 error（app 层据此降级为 nil，平台照常启动）。
func openImpl(src Sources) (Registry, error) {
	if src == nil {
		return nil, errors.New("capability: nil Sources")
	}
	r := &registry{src: src}
	if err := r.refresh(context.Background()); err != nil {
		return nil, err
	}
	return r, nil
}

// List 返回当前快照。detail=false 为渐进式披露第一层：
// 只保留 Name/Kind/Description/Enabled/Source/Unavailable，
// 把 Endpoint/AuthAction/RequiredEnv/Requires/Body 一律清空；
// detail=true 返回全量字段。
func (r *registry) List(_ context.Context, detail bool) ([]Item, error) {
	r.mu.RLock()
	snapshot := r.items
	r.mu.RUnlock()

	out := make([]Item, len(snapshot))
	for i, it := range snapshot {
		c := cloneItem(it)
		if !detail {
			c.Endpoint = ""
			c.AuthAction = ""
			c.RequiredEnv = nil
			c.Requires = nil
			c.Body = ""
		}
		out[i] = c
	}
	return out, nil
}

// Get 返回单项全文（含 Body）。名字精确匹配，不做前缀 / 模糊匹配；
// 找不到返回 ErrNotFound。
func (r *registry) Get(_ context.Context, name string) (Item, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, it := range r.items {
		if it.Name == name {
			return cloneItem(it), nil
		}
	}
	return Item{}, ErrNotFound
}

// Refresh 重新 Collect 并整体替换快照。Collect 在 refreshMu 下串行执行，
// 构建新快照在 mu 之外完成后再一次性换入，读路径几乎不被阻塞。
func (r *registry) Refresh(ctx context.Context) error {
	return r.refresh(ctx)
}

func (r *registry) refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	specs, err := r.src.Collect(ctx)
	if err != nil {
		return err
	}
	items := buildItems(specs)

	r.mu.Lock()
	r.items = items
	r.mu.Unlock()
	return nil
}

// buildItems 把 Spec 固化成不可变 Item：拷出切片、推导 Unavailable、
// 按 Kind 再 Name 排序，保证输出稳定（与 Collect 的返回顺序无关）。
func buildItems(specs []Spec) []Item {
	// 名字 → 是否启用，用于判断 Requires 是否指向快照里明确被禁用的能力。
	enabledByName := make(map[string]bool, len(specs))
	for _, s := range specs {
		if prev, ok := enabledByName[s.Name]; ok {
			enabledByName[s.Name] = prev && s.Enabled
		} else {
			enabledByName[s.Name] = s.Enabled
		}
	}

	items := make([]Item, 0, len(specs))
	for _, s := range specs {
		items = append(items, Item{
			Name:        s.Name,
			Kind:        s.Kind,
			Description: s.Description,
			Enabled:     s.Enabled,
			Source:      s.Source,
			Endpoint:    s.Endpoint,
			AuthAction:  s.AuthAction,
			RequiredEnv: copyStrings(s.RequiredEnv),
			Requires:    copyStrings(s.Requires),
			Body:        s.Body,
			Unavailable: unavailable(s, enabledByName),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Name < items[j].Name
	})
	return items
}

// unavailable 是纯函数：只看快照里的字段，不查环境变量 / 能力开关——
// 开关状态由 Sources.Collect 在装配层折进 Enabled。
//
//   - Enabled=false：优先用 RequiredEnv 写清「需要 MILVUS_ADDR」；
//     没有 env 线索但有 Requires，就写明依赖未就绪；都没有则说明「能力未启用」。
//   - Requires 指向快照里存在但被禁用的能力：即使本条 Enabled=true，
//     也把这条「未满足项」写进原因。
func unavailable(s Spec, enabledByName map[string]bool) string {
	var reasons []string
	if !s.Enabled {
		switch {
		case len(s.RequiredEnv) > 0:
			reasons = append(reasons, "需要 "+strings.Join(s.RequiredEnv, "、"))
		case len(s.Requires) > 0:
			reasons = append(reasons, "依赖未就绪："+strings.Join(s.Requires, "、"))
		default:
			reasons = append(reasons, "能力未启用")
		}
	}
	var unmet []string
	for _, req := range s.Requires {
		if enabled, ok := enabledByName[req]; ok && !enabled {
			unmet = append(unmet, req)
		}
	}
	if len(unmet) > 0 {
		reasons = append(reasons, "依赖被禁用："+strings.Join(unmet, "、"))
	}
	return strings.Join(reasons, "；")
}

// cloneItem 返回 Item 的防御性拷贝，避免调用方改动 RequiredEnv/Requires
// 的底层数组而与缓存共享、产生数据竞争。
func cloneItem(it Item) Item {
	c := it
	c.RequiredEnv = copyStrings(it.RequiredEnv)
	c.Requires = copyStrings(it.Requires)
	return c
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
