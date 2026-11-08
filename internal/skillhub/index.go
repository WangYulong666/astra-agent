package skillhub

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	indexFileName    = "index.json"
	indexFileVersion = 1
)

// indexEntry 是索引里的一条。刻意不含 Body：SKILL.md 正文动辄几 KB，
// 索引是「清单」不是「全集」，正文按需用 Get 读。
type indexEntry struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	Requires    []string  `json:"requires"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	Unavailable string    `json:"unavailable"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type indexFile struct {
	Version int          `json:"version"`
	Skills  []indexEntry `json:"skills"`
}

func (h *hubImpl) indexPath() string { return filepath.Join(h.dir, indexFileName) }

// toIndex 把内存里的 skill 集合序列化成稳定顺序的索引（按名字排序，
// 保证每次重建写出来的 JSON 除了 updatedAt 之外完全一致，便于 diff/审计）。
func (h *hubImpl) toIndex(skills map[string]Skill) indexFile {
	f := indexFile{Version: indexFileVersion, Skills: make([]indexEntry, 0, len(skills))}
	for _, s := range skills {
		req := s.Requires
		if req == nil {
			req = []string{} // 索引里写成 [] 而不是 null，字段始终可读
		}
		f.Skills = append(f.Skills, indexEntry{
			Name:        s.Name,
			Description: s.Description,
			Version:     s.Version,
			Requires:    req,
			Source:      s.Source,
			Status:      s.Status,
			Unavailable: s.Unavailable,
			UpdatedAt:   s.UpdatedAt,
		})
	}
	sort.Slice(f.Skills, func(i, j int) bool { return f.Skills[i].Name < f.Skills[j].Name })
	return f
}

// diskSkill 是扫目录得到的一个候选：body 是 SKILL.md 全文，同时充当
// 「内容变更」的指纹（比对全文比 mtime 可靠——同一秒内覆盖也能发现）。
type diskSkill struct {
	dir  string
	body string
	mod  time.Time
	err  error
}

// scan 扫 SkillDir 的一级子目录，找出带 SKILL.md 的项。
// 返回 map 的 key 是**目录名**（磁盘上的权威标识），与 frontmatter 里的
// name 无关——后者可能被人写错。
func scan(dir string) (map[string]diskSkill, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]diskSkill{}, nil
		}
		return nil, fmt.Errorf("skillhub: 扫描 %s 失败: %w", dir, err)
	}
	out := make(map[string]diskSkill, len(ents))
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			// index.json、.tmp-*、隐藏目录都不是 skill。
			continue
		}
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		info, serr := os.Stat(p)
		if serr != nil {
			if os.IsNotExist(serr) {
				continue // 普通资源目录，不是 skill
			}
			out[e.Name()] = diskSkill{dir: e.Name(), err: serr}
			continue
		}
		b, rerr := os.ReadFile(p)
		out[e.Name()] = diskSkill{dir: e.Name(), body: string(b), mod: info.ModTime(), err: rerr}
	}
	return out, nil
}

// writeFileAtomic 同目录临时文件 + fsync + rename。索引与 SKILL.md 都走它：
// 任何时刻读侧看到的都是完整的旧版或完整的新版。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("skillhub: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("skillhub: 设置权限失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("skillhub: 写文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("skillhub: fsync 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("skillhub: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("skillhub: 落盘失败: %w", err)
	}
	return nil
}
