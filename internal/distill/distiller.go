package distill

// 实现说明（与 distill.go 的设计说明配套）。
//
// 三段流水线，全部是确定性代码，不碰 LLM：
//
//	事件流 → replaySteps（steps.go）→ Step[]
//	Step[] + run.Input → renderBody（render.go）→ SKILL.md 正文
//	Draft → drafts.json（drafts.go）→ 人确认 → Accept 落盘
//
// 唯一的「智能」是把工具名归一化（title / kind / 入参三种来源）和从指令里
// 抠出 slug——两处都用规则兜底，抠不出来就退化成 task-<runID 前 6 位>。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

const (
	maxStepsDefault = 40
	nameMaxLen      = 64
)

// 校验类错误。ErrDisabled / ErrNotFound 在 distill.go 里（冻结）。
var (
	// ErrNotTerminal 表示 run 还在跑，事件流还没写完——这时候沉淀会得到半截步骤。
	ErrNotTerminal = errors.New("distill: run 尚未结束，等它到终态再沉淀")
	// ErrTooFewSteps 表示可复用的步骤不足。纯问答沉淀不出东西，硬生成只是噪音。
	ErrTooFewSteps = errors.New("distill: 成功的工具调用少于 2 步，沉淀不出可复用的技能")
	// ErrBadName 表示技能名非法（只允许 [a-z0-9-]）。
	ErrBadName = errors.New("distill: 技能名只能用小写字母、数字和连字符")
)

type distiller struct {
	st       store.Store
	skillDir string // <SkillDir>
	dataDir  string // 草稿目录：SkillDir 的父目录
	maxSteps int
	enabled  bool

	// mu 保护 drafts.json 的「读—改—写」整体，以及 Accept 的「备份 + 覆盖」。
	// 单进程串行化够用：草稿是控制面本地状态，量级是「人手动点几下」。
	// 落盘本身走临时文件 + rename，所以即使多进程也不会读到半个 JSON。
	mu sync.Mutex
}

var _ Distiller = (*distiller)(nil)

// Open 装配沉淀器。st 用于读 run 与事件流。
//
// Enabled=false 时返回 Disabled 实例（Enabled() 为 false，各方法返回 ErrDisabled）。
// 为什么不是返回 error：能力开关是配置问题，不是启动失败，装配方不该为它写分支。
func Open(cfg Config, st store.Store) (Distiller, error) {
	if !cfg.Enabled {
		return &distiller{}, nil
	}
	if st == nil {
		return nil, fmt.Errorf("distill: 缺少 store，无法读事件流")
	}
	dir := cfg.SkillDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("distill: 无法定位 home 目录: %w", err)
		}
		dir = filepath.Join(home, ".astra", "skills")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("distill: 解析技能目录 %s 失败: %w", dir, err)
	}
	max := cfg.MaxSteps
	if max <= 0 {
		max = maxStepsDefault
	}
	d := &distiller{
		st:       st,
		skillDir: abs,
		dataDir:  filepath.Dir(abs),
		maxSteps: max,
		enabled:  true,
	}
	if err := os.MkdirAll(d.dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("distill: 创建草稿目录 %s 失败: %w", d.dataDir, err)
	}
	// 启动时读一遍：文件坏了要在这里暴露，而不是等用户点了「沉淀」才发现。
	if _, err := d.loadDrafts(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *distiller) Enabled() bool { return d != nil && d.enabled }

func (d *distiller) Distill(ctx context.Context, req Request) (Draft, error) {
	if !d.Enabled() {
		return Draft{}, ErrDisabled
	}
	if strings.TrimSpace(req.RunID) == "" {
		return Draft{}, fmt.Errorf("distill: 缺少 runID")
	}
	run, err := d.st.GetRun(ctx, req.RunID)
	if err != nil {
		return Draft{}, fmt.Errorf("distill: 读 run %s 失败: %w", req.RunID, err)
	}
	if err := d.checkOwnership(ctx, run, req.TenantID); err != nil {
		return Draft{}, err
	}
	if !store.IsTerminal(run.Status) {
		return Draft{}, fmt.Errorf("%w（当前 %s）", ErrNotTerminal, run.Status)
	}

	events, err := d.st.Events(ctx, run.ID, 0, 0)
	if err != nil {
		return Draft{}, fmt.Errorf("distill: 读 run %s 的事件失败: %w", run.ID, err)
	}
	steps, total, omitted := replaySteps(events, d.maxSteps)
	if ok := countOK(steps); ok < minOKSteps {
		return Draft{}, fmt.Errorf("%w（共 %d 次工具调用，%d 次成功）", ErrTooFewSteps, total, ok)
	}

	name := req.Name
	if strings.TrimSpace(name) == "" {
		name = SlugFrom(run.Input, run.ID)
	} else if name, err = NormalizeName(name); err != nil {
		return Draft{}, err
	}

	desc := describe(run)
	draft := Draft{
		ID:          NewID(),
		TenantID:    req.TenantID,
		UserID:      req.UserID,
		RunID:       run.ID,
		SessionID:   firstNonEmpty(run.SessionID, req.SessionID),
		Name:        name,
		Description: desc,
		Body:        renderBody(name, desc, whenToUse(run), steps, total, omitted),
		Steps:       steps,
		Status:      StatusDraft,
		CreatedAt:   time.Now().UTC(),
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	items, err := d.loadDrafts()
	if err != nil {
		return Draft{}, err
	}
	// 同一 run 只保留最新一份未确认草稿：反复点「沉淀」不该堆出一串一样的条目。
	kept := items[:0]
	for _, it := range items {
		if it.RunID == draft.RunID && it.TenantID == draft.TenantID && it.Status == StatusDraft {
			continue
		}
		kept = append(kept, it)
	}
	if err := d.saveDrafts(append(kept, draft)); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

// checkOwnership 拦住跨租户沉淀。
//
// run 本身不带租户，归属挂在 session 上。单租户兼容模式下（session.TenantID 为空）
// 不校验——那时候全库本来就只有一个租户。
func (d *distiller) checkOwnership(ctx context.Context, run store.Run, tenantID string) error {
	if tenantID == "" || run.SessionID == "" {
		return nil
	}
	sess, err := d.st.GetSession(ctx, run.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // session 被删了但事件还在，按归属不明处理
		}
		return fmt.Errorf("distill: 读会话 %s 失败: %w", run.SessionID, err)
	}
	if sess.TenantID != "" && sess.TenantID != tenantID {
		// 报「不存在」而不是「无权限」：不要泄漏别的租户有没有这个 run。
		return ErrNotFound
	}
	return nil
}

func (d *distiller) List(ctx context.Context, tenantID string) ([]Draft, error) {
	if !d.Enabled() {
		return nil, ErrDisabled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	items, err := d.loadDrafts()
	if err != nil {
		return nil, err
	}
	out := make([]Draft, 0, len(items))
	for _, it := range items {
		if it.TenantID == tenantID {
			out = append(out, it)
		}
	}
	// 新的在前：列表页第一眼要看到刚沉淀的那条。
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (d *distiller) Accept(ctx context.Context, tenantID, id string) (string, error) {
	if !d.Enabled() {
		return "", ErrDisabled
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	items, err := d.loadDrafts()
	if err != nil {
		return "", err
	}
	idx := -1
	for i, it := range items {
		if it.ID == id && it.TenantID == tenantID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", ErrNotFound
	}
	draft := items[idx]

	// 名字是唯一会拼进路径的用户可控字段，必须在这里再校验一次：
	// 草稿可能是旧版本写下的、或被手工改过 drafts.json，不能信任入库时的校验。
	name, err := NormalizeName(draft.Name)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrBadName, draft.Name)
	}
	target, err := d.targetPath(name)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("distill: 创建技能目录失败: %w", err)
	}
	// 同名先备份：人可能已经手改过上一版，覆盖前必须留一份能回滚的。
	if _, statErr := os.Stat(target); statErr == nil {
		backup := target + ".bak." + fmt.Sprint(time.Now().Unix())
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("distill: 备份旧技能失败: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("distill: 检查 %s 失败: %w", target, statErr)
	}
	if err := writeFileAtomic(target, []byte(draft.Body), 0o644); err != nil {
		return "", err
	}

	items[idx].Status = StatusAccepted
	items[idx].Path = target
	if err := d.saveDrafts(items); err != nil {
		return "", err
	}
	return target, nil
}

func (d *distiller) Reject(ctx context.Context, tenantID, id string) error {
	if !d.Enabled() {
		return ErrDisabled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	items, err := d.loadDrafts()
	if err != nil {
		return err
	}
	for i := range items {
		if items[i].ID == id && items[i].TenantID == tenantID {
			items[i].Status = StatusRejected
			return d.saveDrafts(items)
		}
	}
	return ErrNotFound
}

// targetPath 返回 <skillDir>/<name>/SKILL.md，并保证结果仍在 skillDir 之内。
//
// NormalizeName 已经把 / \ . 挡掉了，这里再用 filepath.Rel 复核一遍：
// 路径穿越是最容易被一次「顺手放宽正则」重新打开的洞，两道锁成本几乎为零。
func (d *distiller) targetPath(name string) (string, error) {
	dir := filepath.Join(d.skillDir, name)
	rel, err := filepath.Rel(d.skillDir, dir)
	if err != nil || rel != name || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	return filepath.Join(dir, "SKILL.md"), nil
}

// NewID 生成草稿 ID。
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 熵源坏了用时间戳兜底：ID 只要求「本进程内不重」，不需要不可预测。
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
