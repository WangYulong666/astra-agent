package sqlstore

// 审批的 SQL 实现。契约见 internal/store/types.go。
//
// 关键点只有一个：ResolveApproval 必须是原子的条件更新
// （WHERE status = 'pending'），用受影响行数判断谁赢了。
// 审批等于授权 agent 越界执行，两次点击都生效就是越权事故，
// 而进程内锁在多副本下保护不了任何人。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

var _ store.ApprovalStore = (*Store)(nil)

const approvalCols = `run_id, tool_call_id, title, kind, status, created_at, expires_at, resolved_at, resolved_by`

func scanApproval(sc scanner) (store.Approval, error) {
	var (
		a        store.Approval
		created  dbTime
		expires  dbTime
		resolved dbTime
	)
	err := sc.Scan(&a.RunID, &a.ToolCallID, &a.Title, &a.Kind, &a.Status,
		&created, &expires, &resolved, &a.ResolvedBy)
	if err != nil {
		return a, err
	}
	a.CreatedAt, a.ExpiresAt, a.ResolvedAt = created.t, expires.t, resolved.t
	return a, nil
}

func (s *Store) CreateApproval(ctx context.Context, a store.Approval) error {
	if a.RunID == "" || a.ToolCallID == "" {
		return errors.New("sqlstore: 审批缺少 runId / toolCallId")
	}
	if a.Status == "" {
		a.Status = store.ApprovalPending
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.exec(ctx, `INSERT INTO approvals (`+approvalCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, a.ToolCallID, a.Title, a.Kind, a.Status,
		s.timeVal(a.CreatedAt), s.timeVal(a.ExpiresAt), s.timeVal(a.ResolvedAt), a.ResolvedBy)
	if errors.Is(err, store.ErrDuplicate) {
		return fmt.Errorf("%w: 审批 %s/%s 已存在", store.ErrConflict, a.RunID, a.ToolCallID)
	}
	return err
}

func (s *Store) GetApproval(ctx context.Context, runID, toolCallID string) (store.Approval, error) {
	if runID == "" || toolCallID == "" {
		return store.Approval{}, store.ErrNotFound
	}
	a, err := scanApproval(s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+approvalCols+` FROM approvals WHERE run_id = ? AND tool_call_id = ?`),
		runID, toolCallID))
	if err != nil {
		return store.Approval{}, s.mapErr(err)
	}
	return a, nil
}

func (s *Store) ListPendingApprovals(ctx context.Context, runID string) ([]store.Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approvals WHERE status = ?`
	args := []any{store.ApprovalPending}
	if runID != "" {
		q += ` AND run_id = ?`
		args = append(args, runID)
	}
	q += ` ORDER BY created_at ASC, run_id ASC, tool_call_id ASC`

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Approval, 0, 8)
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ResolveApproval(ctx context.Context, runID, toolCallID, status, resolvedBy string, at time.Time) error {
	if !store.ApprovalResolved(status) {
		return fmt.Errorf("sqlstore: 非法审批状态 %q", status)
	}
	res, err := s.exec(ctx, `UPDATE approvals SET status = ?, resolved_at = ?, resolved_by = ?
	WHERE run_id = ? AND tool_call_id = ? AND status = ?`,
		status, s.timeVal(at), resolvedBy, runID, toolCallID, store.ApprovalPending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	// 命中 0 行有两种含义，对前端完全不同：已经处理过（可以忽略）和
	// 根本没有这条（要刷新）。多读一次把两者分开。
	if _, err := s.GetApproval(ctx, runID, toolCallID); err != nil {
		return err
	}
	return fmt.Errorf("%w: 审批 %s/%s 已经处理过", store.ErrConflict, runID, toolCallID)
}

func (s *Store) ExpirePendingApprovals(ctx context.Context, at time.Time) ([]store.Approval, error) {
	rows, err := s.query(ctx, `UPDATE approvals SET status = ?, resolved_at = ?, resolved_by = ?
	WHERE status = ? RETURNING `+approvalCols,
		store.ApprovalExpired, s.timeVal(at), "system", store.ApprovalPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
