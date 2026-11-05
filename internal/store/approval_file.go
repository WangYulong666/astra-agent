package store

// 审批的本地文件实现。
//
// 一个 run 一个 approvals.json（整个数组重写，量是「同时挂起的审批数」，
// 正常不超过个位数）。条件更新的原子性由 f.mu 保证——file store 本来就只
// 支持单进程，跨进程那份正确性由 sqlstore 的 UPDATE ... WHERE status='pending' 承担。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var _ ApprovalStore = (*File)(nil)

func (f *File) approvalPath(runID string) (string, error) {
	dir, err := f.findRun(runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "approvals.json"), nil
}

// readApprovals 读一个 run 的审批列表。文件不存在等同空列表。
func readApprovals(path string) ([]Approval, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Approval
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("store: 审批文件 %s 解析失败: %w", path, err)
	}
	return out, nil
}

func (f *File) CreateApproval(ctx context.Context, a Approval) error {
	if a.RunID == "" || a.ToolCallID == "" {
		return fmt.Errorf("store: 审批缺少 runId / toolCallId")
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	path, err := f.approvalPath(a.RunID)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	list, err := readApprovals(path)
	if err != nil {
		return err
	}
	for _, old := range list {
		if old.ToolCallID == a.ToolCallID {
			return fmt.Errorf("%w: 审批 %s/%s 已存在", ErrConflict, a.RunID, a.ToolCallID)
		}
	}
	list = append(list, a)
	return writeJSON(path, list)
}

func (f *File) GetApproval(ctx context.Context, runID, toolCallID string) (Approval, error) {
	path, err := f.approvalPath(runID)
	if err != nil {
		return Approval{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	list, err := readApprovals(path)
	if err != nil {
		return Approval{}, err
	}
	for _, a := range list {
		if a.ToolCallID == toolCallID {
			return a, nil
		}
	}
	return Approval{}, fmt.Errorf("%w: 审批 %s/%s", ErrNotFound, runID, toolCallID)
}

func (f *File) ListPendingApprovals(ctx context.Context, runID string) ([]Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var paths []string
	if runID != "" {
		p, err := f.approvalPath(runID)
		if err != nil {
			return nil, err
		}
		paths = []string{p}
	} else {
		var err error
		paths, err = f.allApprovalPaths()
		if err != nil {
			return nil, err
		}
	}

	out := make([]Approval, 0, 8)
	for _, p := range paths {
		list, err := readApprovals(p)
		if err != nil {
			return nil, err
		}
		for _, a := range list {
			if a.Status == ApprovalPending {
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		if out[i].RunID != out[j].RunID {
			return out[i].RunID < out[j].RunID
		}
		return out[i].ToolCallID < out[j].ToolCallID
	})
	return out, nil
}

func (f *File) ResolveApproval(ctx context.Context, runID, toolCallID, status, resolvedBy string, at time.Time) error {
	if !ApprovalResolved(status) {
		return fmt.Errorf("store: 非法审批状态 %q", status)
	}
	path, err := f.approvalPath(runID)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	list, err := readApprovals(path)
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ToolCallID != toolCallID {
			continue
		}
		if list[i].Status != ApprovalPending {
			return fmt.Errorf("%w: 审批 %s/%s 已经是 %s", ErrConflict, runID, toolCallID, list[i].Status)
		}
		list[i].Status = status
		list[i].ResolvedAt = at
		list[i].ResolvedBy = resolvedBy
		if err := writeJSON(path, list); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: 审批 %s/%s", ErrNotFound, runID, toolCallID)
}

func (f *File) ExpirePendingApprovals(ctx context.Context, at time.Time) ([]Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	paths, err := f.allApprovalPaths()
	if err != nil {
		return nil, err
	}

	var expired []Approval
	for _, p := range paths {
		list, err := readApprovals(p)
		if err != nil {
			return nil, err
		}
		changed := false
		for i := range list {
			if list[i].Status != ApprovalPending {
				continue
			}
			list[i].Status = ApprovalExpired
			list[i].ResolvedAt = at
			list[i].ResolvedBy = "system"
			expired = append(expired, list[i])
			changed = true
		}
		if changed {
			if err := writeJSON(p, list); err != nil {
				return expired, err
			}
		}
	}
	return expired, nil
}

// allApprovalPaths 枚举 sessions/*/runs/*/approvals.json。调用方必须持有 f.mu。
func (f *File) allApprovalPaths() ([]string, error) {
	sessions, err := os.ReadDir(filepath.Join(f.root, "sessions"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		runsDir := filepath.Join(f.sessionDir(s.Name()), "runs")
		runs, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, r := range runs {
			if !r.IsDir() {
				continue
			}
			out = append(out, filepath.Join(runsDir, r.Name(), "approvals.json"))
		}
	}
	return out, nil
}
