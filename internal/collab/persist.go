package collab

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

func (c *coordinator) childPath() string {
	return filepath.Join(c.cfg.DataDir, "children.json")
}

// load 启动时把磁盘记录读回内存镜像。文件不存在 / 空文件都当作空库；
// 解析不了则让 Open 失败（app 层降级为不可用），避免静默丢数据后又被
// 第一次写入整份覆盖掉。
func (c *coordinator) load() error {
	b, err := os.ReadFile(c.childPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	var items []Child
	if err := json.Unmarshal(b, &items); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range items {
		if ch.RunID != "" {
			c.children[ch.RunID] = ch
		}
	}
	return nil
}

// addOrUpdate 是 children.json 的唯一写入口：锁内改 map、整份序列化落盘。
// 并发 Spawn 因此被串行化，绝不会各自「读旧文件→追加→写回」互相踩坏。
func (c *coordinator) addOrUpdate(ch Child) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.children[ch.RunID] = ch
	return c.flushLocked()
}

// flushLocked 要求调用方已持有 c.mu 写锁。
func (c *coordinator) flushLocked() error {
	items := make([]Child, 0, len(c.children))
	for _, ch := range c.children {
		items = append(items, ch)
	}
	sortChildren(items)
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(c.childPath(), append(b, '\n'), childFilePerm)
}

// sortChildren 按 CreatedAt 正序，同一时刻用 RunID 兜底，保证顺序稳定可复现。
func sortChildren(items []Child) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].RunID < items[j].RunID
		}
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
}

// atomicWrite 临时文件 + fsync + rename。CreateTemp 默认 0600，仍显式 Chmod
// 一次（children.json 含归属与摘要，按契约必须 0600）。临时文件与目标同目录，
// rename 才是同一文件系统内的原子替换；中途任何失败都删掉临时文件。
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
