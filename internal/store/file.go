package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options 控制本地存储的行为。
type Options struct {
	// Sync 每次追加后 fsync。默认开——事件丢了就无法续传，代价只是每批几毫秒。
	Sync bool
}

// File 是单机实现：一个 run 一个 JSONL 文件。
//
// 目录结构（便于人肉排查，不需要任何工具）：
//
//	<root>/sessions/<sid>/session.json
//	<root>/sessions/<sid>/runs/<rid>/run.json
//	<root>/sessions/<sid>/runs/<rid>/events.jsonl
type File struct {
	root string
	opts Options

	mu   sync.Mutex
	runs map[string]*runFile

	closeOnce sync.Once
}

type runFile struct {
	mu      sync.Mutex
	f       *os.File
	nextSeq int64
	dir     string // 所属 run 目录，删会话时用来判断归属
}

// OpenFile 打开（必要时创建）本地存储目录。
func OpenFile(root string, opts Options) (*File, error) {
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		return nil, fmt.Errorf("store: 建目录失败: %w", err)
	}
	return &File{root: root, opts: opts, runs: map[string]*runFile{}}, nil
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validID(id string) error {
	if !idPattern.MatchString(id) {
		// 直接拼进路径的参数必须校验，否则 ../ 能跳出数据目录
		return fmt.Errorf("store: 非法 id %q", id)
	}
	return nil
}

func (f *File) sessionDir(sid string) string { return filepath.Join(f.root, "sessions", sid) }
func (f *File) runDir(sid, rid string) string {
	return filepath.Join(f.sessionDir(sid), "runs", rid)
}

// ── 会话 ─────────────────────────────────────────────────────

func (f *File) CreateSession(ctx context.Context, s Session) error {
	if err := validID(s.ID); err != nil {
		return err
	}
	dir := f.sessionDir(s.ID)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%w: 会话 %s 已存在", ErrConflict, s.ID)
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	if s.LastActiveAt.IsZero() {
		s.LastActiveAt = s.CreatedAt
	}
	if s.Status == "" {
		s.Status = SessionActive
	}
	if err := writeJSON(filepath.Join(dir, "session.json"), s); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(dir, "runs"), 0o755)
}

func (f *File) GetSession(ctx context.Context, id string) (Session, error) {
	var s Session
	if err := validID(id); err != nil {
		return s, err
	}
	if err := readJSON(filepath.Join(f.sessionDir(id), "session.json"), &s); err != nil {
		return s, err
	}
	return s, nil
}

func (f *File) ListSessions(ctx context.Context) ([]Session, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, "sessions"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Session, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var s Session
		if err := readJSON(filepath.Join(f.sessionDir(e.Name()), "session.json"), &s); err != nil {
			continue // 半成品目录直接跳过，不因为一个坏目录让整个列表失败
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *File) PutSession(ctx context.Context, s Session) error {
	if err := validID(s.ID); err != nil {
		return err
	}
	return writeJSON(filepath.Join(f.sessionDir(s.ID), "session.json"), s)
}

func (f *File) DeleteSession(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	dir := f.sessionDir(id)
	f.mu.Lock()
	for rid, rf := range f.runs {
		if strings.HasPrefix(rf.dir, dir+string(os.PathSeparator)) {
			rf.mu.Lock()
			_ = rf.f.Close()
			rf.mu.Unlock()
			delete(f.runs, rid)
		}
	}
	f.mu.Unlock()

	if err := os.RemoveAll(f.sessionDir(id)); err != nil {
		return err
	}
	return nil
}

// ── run ──────────────────────────────────────────────────────

func (f *File) CreateRun(ctx context.Context, r Run) error {
	if err := validID(r.ID); err != nil {
		return err
	}
	if err := validID(r.SessionID); err != nil {
		return err
	}
	dir := f.runDir(r.SessionID, r.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// 事件文件先建出来，Append 才能直接用 O_APPEND
	ef, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer ef.Close()

	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.Status == "" {
		r.Status = RunQueued
	}
	if r.NextSeq == 0 {
		r.NextSeq = 1
	}
	return writeJSON(filepath.Join(dir, "run.json"), r)
}

func (f *File) GetRun(ctx context.Context, id string) (Run, error) {
	var r Run
	path, err := f.findRun(id)
	if err != nil {
		return r, err
	}
	if err := readJSON(filepath.Join(path, "run.json"), &r); err != nil {
		return r, err
	}
	return r, nil
}

func (f *File) ListRuns(ctx context.Context, sessionID string) ([]Run, error) {
	if err := validID(sessionID); err != nil {
		return nil, err
	}
	dir := filepath.Join(f.sessionDir(sessionID), "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Run, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var r Run
		if err := readJSON(filepath.Join(dir, e.Name(), "run.json"), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (f *File) UpdateRun(ctx context.Context, r Run) error {
	path, err := f.findRun(r.ID)
	if err != nil {
		return err
	}
	// 别把内存里可能更新的 nextSeq 写回去覆盖——以已落库事件为准
	f.mu.Lock()
	if rf, ok := f.runs[r.ID]; ok {
		rf.mu.Lock()
		r.NextSeq = rf.nextSeq
		rf.mu.Unlock()
	}
	f.mu.Unlock()
	return writeJSON(filepath.Join(path, "run.json"), r)
}

// findRun 在 sessions/*/runs/<rid> 里定位 run。run id 全局唯一，所以不必先知道会话。
func (f *File) findRun(rid string) (string, error) {
	if err := validID(rid); err != nil {
		return "", err
	}
	sessions, err := os.ReadDir(filepath.Join(f.root, "sessions"))
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		p := filepath.Join(f.sessionDir(s.Name()), "runs", rid)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: run %s", ErrNotFound, rid)
}

// ── 事件 ─────────────────────────────────────────────────────

func (f *File) Append(ctx context.Context, runID string, e NewEvent) (Event, error) {
	rf, err := f.runHandle(runID)
	if err != nil {
		return Event{}, err
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("store: 事件 payload 无法序列化: %w", err)
	}
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage(`{}`)
	}

	rf.mu.Lock()
	defer rf.mu.Unlock()

	ev := Event{
		RunID:     runID,
		Seq:       rf.nextSeq,
		Type:      e.Type,
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return Event{}, err
	}
	if _, err := rf.f.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("store: 写事件失败: %w", err)
	}
	if f.opts.Sync {
		if err := rf.f.Sync(); err != nil {
			return Event{}, fmt.Errorf("store: fsync 失败: %w", err)
		}
	}
	rf.nextSeq++
	return ev, nil
}

func (f *File) Events(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error) {
	path, err := f.findRun(runID)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(filepath.Join(path, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()

	// 坏行（例如进程被 kill 在写一半）跳过而不是让整个回放失败。
	var out []Event
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Seq <= afterSeq {
			continue
		}
		ev.RunID = runID
		out = append(out, ev)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *File) runHandle(runID string) (*runFile, error) {
	f.mu.Lock()
	if rf, ok := f.runs[runID]; ok {
		f.mu.Unlock()
		return rf, nil
	}
	f.mu.Unlock()

	path, err := f.findRun(runID)
	if err != nil {
		return nil, err
	}
	efPath := filepath.Join(path, "events.jsonl")

	// seq 以已落库的最后一条为准：崩溃后重启不能重号，否则主键冲突、
	// 客户端按 after_seq 续传会漏事件。
	next := int64(1)
	if last, err := lastSeq(efPath); err == nil && last >= next {
		next = last + 1
	}

	fh, err := os.OpenFile(efPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	rf := &runFile{f: fh, nextSeq: next, dir: path}

	f.mu.Lock()
	if exist, ok := f.runs[runID]; ok {
		// 并发打开，丢掉自己的句柄
		f.mu.Unlock()
		_ = fh.Close()
		return exist, nil
	}
	f.runs[runID] = rf
	f.mu.Unlock()
	return rf, nil
}

func lastSeq(path string) (int64, error) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer fh.Close()

	var last int64
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Seq > last {
			last = ev.Seq
		}
	}
	return last, sc.Err()
}

func (f *File) Close() error {
	var err error
	f.closeOnce.Do(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for id, rf := range f.runs {
			rf.mu.Lock()
			if e := rf.f.Close(); e != nil {
				err = e
			}
			rf.mu.Unlock()
			delete(f.runs, id)
		}
	})
	return err
}

// ── 小工具 ───────────────────────────────────────────────────

// writeJSON 原子写：先写临时文件再 rename。
// 直接 truncate 原文件的话，写到一半崩溃会留下半截 JSON，等于数据全丢。
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(filepath.Dir(path)))
		}
		return err
	}
	return json.Unmarshal(b, v)
}
