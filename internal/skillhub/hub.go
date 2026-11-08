package skillhub

// 实现说明（与 skillhub.go 的冻结契约配套）。
//
// 目录统一：SkillDir 是唯一权威目录。启动时把编译进二进制的内置 SKILL.md
// 同步进去（已存在不覆盖，人可能手改过），此后 distill.Accept / 手工放置 /
// Publish 都只往这个目录写文件，hub 不要求它们通知自己。
//
// 为什么不用 watcher：fsnotify 要常驻 goroutine、要处理 macOS/容器 FS 的
// 事件丢失与去重，还要把 distill 与 skillhub 用跨包通知接起来。而本目录
// 的文件量是个位数、变更极低频，List/Get 时「扫一次目录 + 比对全文」的
// 代价可以忽略，却能保证任何写文件的方式都立刻可见——包括进程外写入。
// 索引只是这份目录的物化缓存，不是事实来源。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WangYulong666/astra-agent/internal/eventbus"
	"github.com/WangYulong666/astra-agent/internal/store"
)

// ErrNotFound 表示 SkillDir 里没有这个名字的 skill。
var ErrNotFound = errors.New("skillhub: skill 不存在")

const eventSkillPublished = "skill_published"

// hubImpl 的所有可变状态都在 mu 之下。RWMutex：List/Get 的快路径只读内存，
// 只有真需要重建索引时才升级成写锁；并发的读不会互相挡住。
type hubImpl struct {
	dir string
	cfg Config
	bus *eventbus.Hub

	mu         sync.RWMutex
	skills     map[string]Skill  // name -> Skill（含 Body）
	sig        map[string]string // 目录名 -> SKILL.md 全文，用于变更检测
	indexMTime time.Time         // 最近一次写出/读入 index.json 的 mtime
	builtins   map[string]bool   // 内置 skill 名集合，决定 Source
}

var _ Hub = (*hubImpl)(nil)

func openImpl(cfg Config, bus *eventbus.Hub) (Hub, error) {
	dir := cfg.SkillDir
	if dir == "" {
		// 与 distill.Open 的默认完全一致：两边由装配层传同一个 SkillDir，
		// 谁都不自己读环境变量，避免「CLI 一个目录、服务端另一个目录」。
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("skillhub: 无法定位 home 目录: %w", err)
		}
		dir = filepath.Join(home, ".astra", "skills")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("skillhub: 解析技能目录 %s 失败: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("skillhub: 创建技能目录失败: %w", err)
	}

	h := &hubImpl{
		dir:      abs,
		cfg:      cfg,
		bus:      bus,
		skills:   map[string]Skill{},
		sig:      map[string]string{},
		builtins: map[string]bool{},
	}
	if err := h.syncBuiltins(); err != nil {
		return nil, err
	}
	// 启动时建一次索引（不广播：启动不是「发布」动作，别刷一屏事件）。
	h.mu.Lock()
	_, err = h.rebuildLocked()
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return h, nil
}

// syncBuiltins 把内置 SKILL.md 同步进 SkillDir，已存在的一律跳过。
func (h *hubImpl) syncBuiltins() error {
	builtin, err := builtinSkills()
	if err != nil {
		return fmt.Errorf("skillhub: 读内置 skill 失败: %w", err)
	}
	for name, body := range builtin {
		if err := h.syncOne(name, body); err != nil {
			return err
		}
		h.builtins[name] = true
	}
	// BuiltinDir 是可选的额外来源（例如发行版自带的更多 skill）。同样是
	// 「不覆盖」语义；embed 先写，所以内置版本优先。
	if p := strings.TrimSpace(h.cfg.BuiltinDir); p != "" {
		matches, err := filepath.Glob(filepath.Join(p, "*", "SKILL.md"))
		if err != nil {
			return fmt.Errorf("skillhub: 扫描 BuiltinDir %s 失败: %w", p, err)
		}
		for _, m := range matches {
			name := filepath.Base(filepath.Dir(m))
			if !validName(name) {
				continue
			}
			body, err := os.ReadFile(m)
			if err != nil {
				return fmt.Errorf("skillhub: 读 %s 失败: %w", m, err)
			}
			if err := h.syncOne(name, body); err != nil {
				return err
			}
			h.builtins[name] = true
		}
	}
	return nil
}

func (h *hubImpl) syncOne(name string, body []byte) error {
	target := filepath.Join(h.dir, name, "SKILL.md")
	if _, err := os.Stat(target); err == nil {
		return nil // 已存在：人可能手改过，绝不覆盖
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("skillhub: 检查 %s 失败: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("skillhub: 创建 %s 失败: %w", filepath.Dir(target), err)
	}
	return writeFileAtomic(target, body, 0o644)
}

// ── List / Get / Refresh / Publish ────────────────────────────

func (h *hubImpl) List(ctx context.Context) ([]Skill, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, news, err := h.ensureFresh()
	if err != nil {
		return nil, err
	}
	h.broadcast(news)
	return out, nil
}

func (h *hubImpl) Get(ctx context.Context, name string) (Skill, error) {
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	_, news, err := h.ensureFresh()
	if err != nil {
		return Skill{}, err
	}
	h.broadcast(news)
	h.mu.RLock()
	s, ok := h.skills[name]
	h.mu.RUnlock()
	if !ok {
		return Skill{}, ErrNotFound
	}
	return s, nil
}

func (h *hubImpl) Refresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	news, err := h.rebuildLocked()
	h.mu.Unlock()
	if err != nil {
		return err
	}
	h.broadcast(news)
	return nil
}

func (h *hubImpl) Publish(ctx context.Context, name, body string, requires []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	n := strings.TrimSpace(name)
	if !validName(n) {
		return "", fmt.Errorf("skillhub: 技能名 %q 不合法（只允许 [a-z0-9-]）", name)
	}
	// 再复核一次路径：validName 已经挡掉 / \ .，两道锁成本几乎为零。
	dir := filepath.Join(h.dir, n)
	if rel, err := filepath.Rel(h.dir, dir); err != nil || rel != n || strings.Contains(rel, "..") {
		return "", fmt.Errorf("skillhub: 技能名 %q 不合法", name)
	}
	target := filepath.Join(dir, "SKILL.md")
	final := ensureRequires(body, requires)

	h.mu.Lock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.mu.Unlock()
		return "", fmt.Errorf("skillhub: 创建技能目录失败: %w", err)
	}
	// 同名覆盖前留 .bak.<unix>，照 distill.Accept 的做法。
	if _, err := os.Stat(target); err == nil {
		backup := target + ".bak." + fmt.Sprint(time.Now().Unix())
		if err := os.Rename(target, backup); err != nil {
			h.mu.Unlock()
			return "", fmt.Errorf("skillhub: 备份旧技能失败: %w", err)
		}
	} else if !os.IsNotExist(err) {
		h.mu.Unlock()
		return "", fmt.Errorf("skillhub: 检查 %s 失败: %w", target, err)
	}
	if err := writeFileAtomic(target, []byte(final), 0o644); err != nil {
		h.mu.Unlock()
		return "", err
	}
	news, err := h.rebuildLocked()
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	// Publish 是显式发布：覆盖已有 skill 也要广播，所以确保 name 在事件里。
	found := false
	for _, s := range news {
		if s.Name == n {
			found = true
			break
		}
	}
	if !found {
		news = append(news, Skill{Name: n, Source: h.sourceFor(n)})
	}
	h.broadcast(news)
	return target, nil
}

// ── 懒刷新 ───────────────────────────────────────────────────

// ensureFresh 返回当前清单与本次「新发现」的 skill。
// 快路径持 RLock；发现变更才升级成 Lock 重建（双检查防并发重复重建）。
func (h *hubImpl) ensureFresh() ([]Skill, []Skill, error) {
	h.mu.RLock()
	if h.freshLocked() {
		out := h.skillsCopyLocked()
		h.mu.RUnlock()
		return out, nil, nil
	}
	h.mu.RUnlock()

	h.mu.Lock()
	if h.freshLocked() {
		out := h.skillsCopyLocked()
		h.mu.Unlock()
		return out, nil, nil
	}
	news, err := h.rebuildLocked()
	var out []Skill
	if err == nil {
		out = h.skillsCopyLocked()
	}
	h.mu.Unlock()
	return out, news, err
}

// freshLocked 判断内存缓存是否仍与磁盘一致。调用方须持有 RLock 或 Lock。
func (h *hubImpl) freshLocked() bool {
	disk, err := scan(h.dir)
	if err != nil {
		return false
	}
	if len(disk) != len(h.sig) {
		return false
	}
	for name, ds := range disk {
		if h.sig[name] != ds.body {
			return false
		}
	}
	// index.json 自身也要在、且没被外部改过：改过就重建，目录才是权威。
	info, err := os.Stat(h.indexPath())
	if err != nil || !info.ModTime().Equal(h.indexMTime) {
		return false
	}
	return true
}

// rebuildLocked 从目录重建内存清单 + index.json。调用方须持有 Lock。
// 返回本次新出现（此前索引里没有）的 skill，供广播用。
func (h *hubImpl) rebuildLocked() ([]Skill, error) {
	disk, err := scan(h.dir)
	if err != nil {
		return nil, err
	}
	old := h.skills
	next := make(map[string]Skill, len(disk))
	sig := make(map[string]string, len(disk))
	for dir, ds := range disk {
		sig[dir] = ds.body
		s := h.buildSkill(ds)
		next[s.Name] = s
	}

	b, err := json.MarshalIndent(h.toIndex(next), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("skillhub: 序列化索引失败: %w", err)
	}
	if err := writeFileAtomic(h.indexPath(), append(b, '\n'), 0o600); err != nil {
		return nil, err
	}
	if info, err := os.Stat(h.indexPath()); err == nil {
		h.indexMTime = info.ModTime()
	}
	h.skills, h.sig = next, sig

	var news []Skill
	for name, s := range next {
		if _, ok := old[name]; !ok {
			news = append(news, s)
		}
	}
	sort.Slice(news, func(i, j int) bool { return news[i].Name < news[j].Name })
	return news, nil
}

func (h *hubImpl) skillsCopyLocked() []Skill {
	out := make([]Skill, 0, len(h.skills))
	for _, s := range h.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// broadcast 在锁外发布。eventbus 的全局回调是同步执行的，这里只做一次
// Publish（不读文件、不写盘），把「重活」留给订阅方自己决定。
func (h *hubImpl) broadcast(news []Skill) {
	if h.bus == nil || len(news) == 0 {
		return
	}
	for _, s := range news {
		payload, err := json.Marshal(struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		}{Name: s.Name, Source: s.Source})
		if err != nil {
			continue
		}
		h.bus.Publish(store.Event{
			Type:      eventSkillPublished,
			Payload:   payload,
			CreatedAt: time.Now().UTC(),
		})
	}
}

// ── 单条 skill 的构造与 requires 校验 ─────────────────────────

func (h *hubImpl) buildSkill(ds diskSkill) Skill {
	updated := ds.mod
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	s := Skill{
		Name:      ds.dir,
		Source:    h.sourceFor(ds.dir),
		Status:    "ready",
		Body:      ds.body,
		UpdatedAt: updated,
	}
	// 任何一条不满足都保留条目、写明原因，绝不静默丢掉。
	fail := func(format string, args ...any) Skill {
		s.Status = "unavailable"
		s.Unavailable = fmt.Sprintf(format, args...)
		return s
	}
	if ds.err != nil {
		return fail("SKILL.md 读取失败: %v", ds.err)
	}
	fm, err := parseFrontMatter(ds.body)
	if err != nil {
		return fail("frontmatter 格式错误: %v", err)
	}
	s.Description, s.Version, s.Requires = fm.Description, fm.Version, fm.Requires

	name := fm.Name
	if name == "" {
		name = ds.dir // 与 CLI 一致：没写 name 就用目录名
	}
	if !validName(name) {
		return fail("技能名 %q 不合法（只允许 [a-z0-9-]）", name)
	}
	s.Name = name
	s.Status, s.Unavailable = h.validate(fm.Requires)
	return s
}

func (h *hubImpl) sourceFor(dir string) string {
	if h.builtins[dir] {
		return "builtin"
	}
	return "distilled"
}

// validate 判定 requires 是否满足，返回 status 与不可用原因。
// 任一不满足即整体 unavailable，并给出第一条命中的原因。
func (h *hubImpl) validate(requires []string) (string, string) {
	for _, r := range requires {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if r == "astra-cli" {
			if !cliExecutable(h.cfg.CLIBin) {
				return "unavailable", "astra-cli 不可用"
			}
			continue
		}
		if !h.cfg.Switches[r] {
			return "unavailable", fmt.Sprintf("能力 %s 未启用", r)
		}
	}
	return "ready", ""
}

// cliExecutable 按契约只认 Config.CLIBin：非空、存在、非目录、有执行位。
func cliExecutable(bin string) bool {
	if strings.TrimSpace(bin) == "" {
		return false
	}
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
