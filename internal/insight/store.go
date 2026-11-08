package insight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ErrNotFound 是 ForRun 找不到对应诊断时返回的错误。
// 单独定义（而不复用 store.ErrNotFound）：调用方可以用 errors.Is 把
// 「没有诊断」和平台存储的「没有实体」区分开。
var ErrNotFound = errors.New("insight: 诊断不存在")

const (
	insightsFileName   = "insights.json"
	maxItems           = 500
	defaultRecentLimit = 20
	defaultAllLimit    = 50
)

// fileStore 是 insights.json 的进程内视图。
//
// 为什么按路径共享实例：app 层用 Open(dir) 拿到的 Store 供 /v1/insights 与
// StartRun 回灌读取，Attach 写诊断用的是同一个 dir。两者必须是同一个实例，
// 否则 Open 的内存缓存永远看不到 Attach 刚写的记录（回灌会一直读到旧数据）。
// registry 按绝对路径保证这一点；进程内单例，也符合架构文档「单机文件」的定位。
type fileStore struct {
	path string

	mu    sync.RWMutex
	items []Diagnosis // 写入顺序：最旧在前、最新在后；上限 maxItems，超出丢最旧（FIFO）
}

var (
	registryMu sync.Mutex
	registry   = map[string]*fileStore{}
)

// sharedStore 返回（必要时创建）某个 dataDir 对应的唯一 fileStore。
func sharedStore(dataDir string) (*fileStore, error) {
	if dataDir == "" {
		return nil, errors.New("insight: dataDir 为空")
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("insight: 解析 dataDir 失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("insight: 建数据目录失败: %w", err)
	}
	path := filepath.Join(abs, insightsFileName)

	registryMu.Lock()
	defer registryMu.Unlock()
	if s, ok := registry[path]; ok {
		return s, nil
	}
	items, err := loadInsights(path)
	if err != nil {
		return nil, err
	}
	s := &fileStore{path: path, items: items}
	registry[path] = s
	return s, nil
}

func loadInsights(path string) ([]Diagnosis, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // 缺文件是正常首启
	}
	if err != nil {
		return nil, fmt.Errorf("insight: 读 %s 失败: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil // 空文件没有数据可丢，按空列表处理
	}
	var items []Diagnosis
	if err := json.Unmarshal(b, &items); err != nil {
		// 坏 JSON 必须报错：静默当成空列表会把已有诊断全部抹掉且无人察觉。
		return nil, fmt.Errorf("insight: %s 损坏，拒绝静默清空: %w", path, err)
	}
	return items, nil
}

// append 追加一条诊断并原子落盘。并发调用由 mu 串行化，多个诊断
// goroutine 不会互相写坏文件（临时文件名唯一 + rename 原子）。
func (s *fileStore) append(d Diagnosis) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make([]Diagnosis, 0, len(s.items)+1)
	next = append(next, s.items...)
	next = append(next, d)
	if len(next) > maxItems {
		next = next[len(next)-maxItems:] // 丢最旧（FIFO）
	}
	if err := writeInsights(s.path, next); err != nil {
		return err
	}
	s.items = next // 只在写盘成功后更新内存，保证缓存与磁盘一致
	return nil
}

// writeInsights 原子写：唯一临时文件 → 写 → fsync → rename → fsync 目录。
func writeInsights(path string, items []Diagnosis) error {
	b, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("insight: 序列化诊断失败: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("insight: 建数据目录失败: %w", err)
	}
	f, err := os.CreateTemp(dir, ".insights-*.tmp")
	if err != nil {
		return fmt.Errorf("insight: 建临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功后 Remove 返回 not-exist，忽略

	// CreateTemp 默认就是 0600，显式再设一次：诊断里可能带错误文本与路径。
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("insight: 设置文件权限失败: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil { // fsync 文件内容
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("insight: rename 失败: %w", err)
	}
	// 让 rename 这个目录项也落盘（best-effort：部分文件系统不支持目录 fsync）。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Recent 按 sessionID 过滤、CreatedAt 倒序；limit<=0 用 20。
func (s *fileStore) Recent(ctx context.Context, sessionID string, limit int) ([]Diagnosis, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	s.mu.RLock()
	out := make([]Diagnosis, 0, len(s.items))
	for _, d := range s.items {
		if d.SessionID == sessionID {
			out = append(out, d) // Diagnosis 全是值字段，拷贝安全
		}
	}
	s.mu.RUnlock()
	sortByCreatedAtDesc(out)
	return trim(out, limit), nil
}

// All 全量 CreatedAt 倒序；limit<=0 用 50。
func (s *fileStore) All(ctx context.Context, limit int) ([]Diagnosis, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultAllLimit
	}
	s.mu.RLock()
	out := make([]Diagnosis, len(s.items))
	copy(out, s.items) // 返回副本，外部改不坏缓存
	s.mu.RUnlock()
	sortByCreatedAtDesc(out)
	return trim(out, limit), nil
}

// ForRun 精确查找；找不到返回 ErrNotFound。
func (s *fileStore) ForRun(ctx context.Context, runID string) (Diagnosis, error) {
	if err := ctx.Err(); err != nil {
		return Diagnosis{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.items {
		if d.RunID == runID {
			return d, nil // 值拷贝
		}
	}
	return Diagnosis{}, ErrNotFound
}

func sortByCreatedAtDesc(in []Diagnosis) {
	sort.Slice(in, func(i, j int) bool { return in[i].CreatedAt.After(in[j].CreatedAt) })
}

func trim(in []Diagnosis, limit int) []Diagnosis {
	if len(in) > limit {
		return in[:limit]
	}
	return in
}
