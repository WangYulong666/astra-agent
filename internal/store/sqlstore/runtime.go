package sqlstore

// 运行时存储：会话、run、事件。契约见 internal/store/types.go。
//
// 这里与 file store 的行为刻意对齐（错误语义、默认值、排序方向），
// 因为上层是按 file store 的行为写的，换后端不该让 API 表现变化。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

const sessionCols = `id, tenant_id, user_id, sandbox_id, agent_session_id, title, cwd, status, created_at, last_active_at`

func scanSession(sc scanner) (store.Session, error) {
	var (
		s          store.Session
		created    dbTime
		lastActive dbTime
	)
	err := sc.Scan(&s.ID, &s.TenantID, &s.UserID, &s.SandboxID, &s.AgentSessionID,
		&s.Title, &s.Cwd, &s.Status, &created, &lastActive)
	if err != nil {
		return s, err
	}
	s.CreatedAt, s.LastActiveAt = created.t, lastActive.t
	return s, nil
}

func (s *Store) CreateSession(ctx context.Context, sess store.Session) error {
	if sess.ID == "" {
		return errors.New("sqlstore: 会话 id 不能为空")
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	if sess.LastActiveAt.IsZero() {
		sess.LastActiveAt = sess.CreatedAt
	}
	if sess.Status == "" {
		sess.Status = store.SessionActive
	}

	_, err := s.exec(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.TenantID, sess.UserID, sess.SandboxID, sess.AgentSessionID,
		sess.Title, sess.Cwd, sess.Status, s.timeVal(sess.CreatedAt), s.timeVal(sess.LastActiveAt))
	if errors.Is(err, store.ErrDuplicate) {
		// 主键冲突在 file store 里是 ErrConflict（"已存在"），这里跟随它，
		// 免得同一个用例在两个后端上得到不同的错误，上层要写两套判断。
		return fmt.Errorf("%w: 会话 %s 已存在", store.ErrConflict, sess.ID)
	}
	return err
}

func (s *Store) GetSession(ctx context.Context, id string) (store.Session, error) {
	if id == "" {
		return store.Session{}, store.ErrNotFound
	}
	sess, err := scanSession(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+sessionCols+` FROM sessions WHERE id = ?`), id))
	if err != nil {
		return store.Session{}, s.mapErr(err)
	}
	return sess, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]store.Session, error) {
	rows, err := s.query(ctx, `SELECT `+sessionCols+` FROM sessions ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return collectSessions(rows)
}

// PutSession 是覆盖写（upsert）。调用方会在会话存续期间反复更新
// last_active_at / title，没有它就只剩「删了重建」这一条路，会连带丢掉 run。
func (s *Store) PutSession(ctx context.Context, sess store.Session) error {
	if sess.ID == "" {
		return errors.New("sqlstore: 会话 id 不能为空")
	}
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	if sess.LastActiveAt.IsZero() {
		sess.LastActiveAt = sess.CreatedAt
	}
	if sess.Status == "" {
		sess.Status = store.SessionActive
	}
	_, err := s.exec(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
	tenant_id = excluded.tenant_id,
	user_id = excluded.user_id,
	sandbox_id = excluded.sandbox_id,
	agent_session_id = excluded.agent_session_id,
	title = excluded.title,
	cwd = excluded.cwd,
	status = excluded.status,
	created_at = excluded.created_at,
	last_active_at = excluded.last_active_at`,
		sess.ID, sess.TenantID, sess.UserID, sess.SandboxID, sess.AgentSessionID,
		sess.Title, sess.Cwd, sess.Status, s.timeVal(sess.CreatedAt), s.timeVal(sess.LastActiveAt))
	return err
}

// DeleteSession 删会话及其所有 run / 事件（靠外键 ON DELETE CASCADE）。
// 不存在的 id 视作成功，与 file store 的 os.RemoveAll 一致。
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ── run ──────────────────────────────────────────────────────

const runCols = `id, session_id, status, input, stop_reason, error, next_seq, created_at, started_at, ended_at`

func scanRun(sc scanner) (store.Run, error) {
	var (
		r       store.Run
		created dbTime
		started dbTime
		ended   dbTime
	)
	err := sc.Scan(&r.ID, &r.SessionID, &r.Status, &r.Input, &r.StopReason, &r.Error,
		&r.NextSeq, &created, &started, &ended)
	if err != nil {
		return r, err
	}
	r.CreatedAt, r.StartedAt, r.EndedAt = created.t, started.t, ended.t
	return r, nil
}

func (s *Store) CreateRun(ctx context.Context, r store.Run) error {
	if r.ID == "" || r.SessionID == "" {
		return errors.New("sqlstore: run id 与会话 id 不能为空")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = store.RunQueued
	}
	if r.NextSeq == 0 {
		r.NextSeq = 1
	}
	_, err := s.exec(ctx, `INSERT INTO runs (`+runCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.SessionID, r.Status, r.Input, r.StopReason, r.Error,
		r.NextSeq, s.timeVal(r.CreatedAt), s.timeVal(r.StartedAt), s.timeVal(r.EndedAt))
	if errors.Is(err, store.ErrDuplicate) {
		return fmt.Errorf("%w: run %s 已存在", store.ErrConflict, r.ID)
	}
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (store.Run, error) {
	if id == "" {
		return store.Run{}, store.ErrNotFound
	}
	r, err := scanRun(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+runCols+` FROM runs WHERE id = ?`), id))
	if err != nil {
		return store.Run{}, s.mapErr(err)
	}
	return r, nil
}

func (s *Store) ListRuns(ctx context.Context, sessionID string) ([]store.Run, error) {
	rows, err := s.query(ctx, `SELECT `+runCols+` FROM runs WHERE session_id = ? ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Run, 0, 8)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRun 只覆盖元数据，next_seq 不在更新列表里：
// 它是 Append 的私有计数器，调用方手里的 Run 副本通常是旧的，
// 写回去会把已分配过的 seq 倒退（file store 里那段「以已落库事件为准」的逻辑）。
func (s *Store) UpdateRun(ctx context.Context, r store.Run) error {
	q := `UPDATE runs SET status = ?, input = ?, stop_reason = ?, error = ?,
	started_at = ?, ended_at = ? WHERE id = ?`
	args := []any{
		r.Status, r.Input, r.StopReason, r.Error,
		s.timeVal(r.StartedAt), s.timeVal(r.EndedAt), r.ID,
	}
	// 终态不可逆：非终态的写入不覆盖已落库的终态。
	//
	// 与 file store 同一个道理：审批回调（ACP 读循环协程）手里的 run 是旧快照，
	// 它把状态置回 running 时，收尾可能已经写了 cancelled。条件更新让数据库
	// 自己判定谁该赢，而不是靠调用方的时序。
	if !store.IsTerminal(r.Status) {
		q += ` AND status NOT IN ('completed', 'failed', 'cancelled')`
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// 可能是「没有这条 run」，也可能是「被终态保护拦下了」——后者是幂等成功。
		var cur string
		if qerr := s.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, r.ID).Scan(&cur); qerr == nil {
			return nil
		}
		return fmt.Errorf("%w: run %s", store.ErrNotFound, r.ID)
	}
	return nil
}

// ── 事件 ─────────────────────────────────────────────────────

// Append 分配 seq 并写入事件。
//
// 并发正确性靠一条 UPDATE ... RETURNING：
//
//	UPDATE runs SET next_seq = next_seq + 1 WHERE id = ? RETURNING next_seq
//
// 它在数据库里持有该行写锁到事务结束，另一个事务必须等它提交才能读到新值，
// 所以不可能出现两个 Append 拿到同一个 seq。
// 绝对不能写成「先 SELECT next_seq，再 +1 写回」——那是典型的读改写竞态，
// 在 file store 里还能靠单进程互斥锁兜住，跨进程部署就必然重号。
func (s *Store) Append(ctx context.Context, runID string, e store.NewEvent) (store.Event, error) {
	var zero store.Event

	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return zero, fmt.Errorf("store: 事件 payload 无法序列化: %w", err)
	}
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage(`{}`) // nil / 无字段的 payload 统一成 {}，前端不必判空
	}

	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	// 提交后 Rollback 返回 ErrTxDone，忽略即可
	defer func() { _ = tx.Rollback() }()

	var next int64
	err = tx.QueryRowContext(ctx, s.rebind(
		`UPDATE runs SET next_seq = next_seq + 1 WHERE id = ? RETURNING next_seq`), runID).Scan(&next)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// UPDATE 命中 0 行：run 不存在
			return zero, fmt.Errorf("%w: run %s", store.ErrNotFound, runID)
		}
		return zero, s.mapErr(err)
	}

	ev := store.Event{
		RunID:     runID,
		Seq:       next - 1, // next_seq 是「下一个待分配」，本次拿到的是它的前一个
		Type:      e.Type,
		Payload:   payload,
		CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, s.rebind(
		`INSERT INTO run_events (run_id, seq, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`),
		runID, ev.Seq, ev.Type, string(payload), s.timeVal(now)); err != nil {
		return zero, s.mapErr(err)
	}
	if err := tx.Commit(); err != nil {
		return zero, s.mapErr(err)
	}
	return ev, nil
}

func (s *Store) Events(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	q := `SELECT seq, type, payload, created_at FROM run_events WHERE run_id = ? AND seq > ? ORDER BY seq ASC`
	args := []any{runID, afterSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Event, 0, 16)
	for rows.Next() {
		var (
			ev      store.Event
			payload string
			created dbTime
		)
		if err := rows.Scan(&ev.Seq, &ev.Type, &payload, &created); err != nil {
			return nil, err
		}
		ev.RunID = runID
		ev.Payload = json.RawMessage(payload)
		ev.CreatedAt = created.t
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// SQLite 池只有一条连接，下面那次 GetRun 之前必须显式归还，
	// 不能指望遍历自然结束后的隐式释放（defer 要等函数返回才执行）。
	rows.Close()

	if len(out) == 0 {
		// 空结果要区分「run 不存在」和「run 存在但没有更多事件」：
		// 前者必须回 ErrNotFound，否则调用方会把拼错的 run id 当成正常空流。
		if _, err := s.GetRun(ctx, runID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ── 收集辅助 ─────────────────────────────────────────────────

func collectSessions(rows *sql.Rows) ([]store.Session, error) {
	defer rows.Close()
	out := make([]store.Session, 0, 8)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
