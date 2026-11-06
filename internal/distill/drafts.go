package distill

// 草稿持久化：<dataDir>/drafts.json。
//
// 为什么是单文件：草稿是「人点几下就产生一条」的低频数据，量级几十条，
// 查询模式只有「按租户列」和「按 id 找」。为它建表/起依赖没有收益，
// 而单文件在 file store / sql store 两种部署下都能用。
//
// 写入一律「临时文件 + fsync + rename」：任何时刻磁盘上的 drafts.json
// 要么是完整的旧版、要么是完整的新版，崩溃不会留下半个 JSON 让人丢光所有草稿。
// 权限 0600：正文里可能带用户指令原文与路径，属于租户内部信息。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const draftsVersion = 1

type draftsFile struct {
	Version int     `json:"version"`
	Items   []Draft `json:"items"`
}

func (d *distiller) draftsPath() string { return filepath.Join(d.dataDir, "drafts.json") }

// loadDrafts 读草稿。文件不存在等于空列表（首次启动的正常状态）。
// 调用方必须持有 d.mu。
func (d *distiller) loadDrafts() ([]Draft, error) {
	b, err := os.ReadFile(d.draftsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("distill: 读草稿文件失败: %w", err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var f draftsFile
	if err := json.Unmarshal(b, &f); err != nil {
		// 不静默重置：草稿里有人工确认前的产物，覆盖掉等于让用户白干。
		return nil, fmt.Errorf("distill: 草稿文件 %s 已损坏（%v），请人工修复或移走", d.draftsPath(), err)
	}
	return f.Items, nil
}

// saveDrafts 覆盖写草稿文件。调用方必须持有 d.mu。
func (d *distiller) saveDrafts(items []Draft) error {
	if items == nil {
		items = []Draft{}
	}
	b, err := json.MarshalIndent(draftsFile{Version: draftsVersion, Items: items}, "", "  ")
	if err != nil {
		return fmt.Errorf("distill: 序列化草稿失败: %w", err)
	}
	if err := os.MkdirAll(d.dataDir, 0o700); err != nil {
		return fmt.Errorf("distill: 创建草稿目录失败: %w", err)
	}
	return writeFileAtomic(d.draftsPath(), append(b, '\n'), 0o600)
}

// writeFileAtomic 先写同目录临时文件再 rename，保证读侧永远看到完整内容。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("distill: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	defer func() {
		// rename 成功后这里 os.IsNotExist，失败时把半成品清掉。
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("distill: 设置权限失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("distill: 写文件失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("distill: fsync 失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("distill: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("distill: 落盘失败: %w", err)
	}
	return nil
}
