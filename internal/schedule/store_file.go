package schedule

// 单文件 JSON 持久化。
//
// 为什么不是数据库：schedule 的量级是「每人几十条」，查询模式只有
// 「按租户列」和「按 next_run_at 扫」，不值得为它引入一张表；
// 而独立成文件能在 file store / sql store 两种部署下都工作。
//
// 并发：进程内 sync.RWMutex（快路径）+ 跨进程 flock（慢路径）。
// 写全部走「临时文件 + rename」，所以任何时刻磁盘上的文件都是完整的旧版或新版，
// 读侧不需要额外加锁也不会读到半个 JSON。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrQuota 表示单租户条目数超过 MaxPerTenant。api 层回 409。
var ErrQuota = errors.New("schedule: 单租户定时任务数量已达上限")

// lockRetries 是快路径的重试次数（含首次）。
const lockRetries = 3

// lockSlowWait 是慢路径的总等待上限。
//
// 为什么快路径之外还要有慢路径：对面如果正在密集写（一次写包含两次 fsync，
// 本机实测 ~13ms），它释放锁之后会立刻再抢，非阻塞的 3 次重试很可能全部落在
// 对方持锁的窗口里——于是「并发写」被误判成「拿不到锁」。
// 慢路径用更细的抖动探针重试，把冲突摊平，同时仍有硬上限，不会永久挂住。
const lockSlowWait = 2 * time.Second

// fileStore 是 Store 的文件实现。
type fileStore struct {
	path string // <dataDir>/schedules.json
	lock string // <dataDir>/schedules.lock
	max  int    // MaxPerTenant

	mu sync.RWMutex
}

var _ Store = (*fileStore)(nil)

// diskFile 是落盘结构。带 version 是为了以后改字段时有地方判断，
// 现在读到任何 version 都按 1 处理（字段都是可选的，向前兼容）。
type diskFile struct {
	Version int        `json:"version"`
	Items   []Schedule `json:"items"`
}

const diskVersion = 1

func openFileStore(dataDir string, maxPerTenant int) (*fileStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("schedule: 创建数据目录 %s 失败: %w", dataDir, err)
	}
	s := &fileStore{
		path: filepath.Join(dataDir, "schedules.json"),
		lock: filepath.Join(dataDir, "schedules.lock"),
		max:  maxPerTenant,
	}
	// 启动时读一遍：文件损坏要在这里就暴露，而不是等第一次触发。
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// ── 加锁与读写 ───────────────────────────────────────────────

// acquire 拿跨进程文件锁。先重试 3 次，仍撞上就走有界的慢路径，最后才报错。
//
// 用 LOCK_NB + 自旋而不是阻塞 Flock：阻塞版一旦对面进程挂了（或 NFS 抽风），
// 这里就无声地停住，比报错难排查得多。
func (s *fileStore) acquire(exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(s.lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("schedule: 打开锁文件 %s 失败: %w", s.lock, err)
	}
	var last error
	for i := 0; i < lockRetries; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 20 * time.Millisecond)
		}
		if err := flockFile(f.Fd(), exclusive); err == nil {
			return f, nil
		} else {
			last = err
		}
	}
	// 慢路径：抖动探针，把「对方永远刚好持锁」这种相位冲突摊平。
	deadline := time.Now().Add(lockSlowWait)
	for {
		if err := flockFile(f.Fd(), exclusive); err == nil {
			return f, nil
		} else {
			last = err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("schedule: 重试 %d 次并在 %s 内未拿到文件锁 %s: %w",
				lockRetries, lockSlowWait, s.lock, last)
		}
		time.Sleep(time.Duration(1+time.Now().UnixNano()%5) * time.Millisecond)
	}
}

func releaseLock(f *os.File) {
	_ = funlockFile(f.Fd())
	_ = f.Close()
}

// read 在读锁下读整份文件，fn 不应修改 df。
func (s *fileStore) read(ctx context.Context, fn func(*diskFile) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	lk, err := s.acquire(false)
	if err != nil {
		return err
	}
	defer releaseLock(lk)
	df, err := s.load()
	if err != nil {
		return err
	}
	return fn(df)
}

// update 在写锁下读改写。fn 返回错误则**不落盘**（整体回滚）。
func (s *fileStore) update(ctx context.Context, fn func(*diskFile) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, err := s.acquire(true)
	if err != nil {
		return err
	}
	defer releaseLock(lk)
	df, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(df); err != nil {
		return err
	}
	return s.flush(df)
}

func (s *fileStore) load() (*diskFile, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &diskFile{Version: diskVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("schedule: 读 %s 失败: %w", s.path, err)
	}
	var df diskFile
	if err := json.Unmarshal(b, &df); err != nil {
		// 不静默重置：那等于把用户的定时任务全部删掉。
		return nil, fmt.Errorf("schedule: %s 不是合法 JSON，拒绝覆盖（请人工修复或挪走）: %w", s.path, err)
	}
	if df.Version == 0 {
		df.Version = diskVersion
	}
	return &df, nil
}

// flush 原子落盘：临时文件 → fsync → rename → fsync 目录。
//
// 少了最后那个目录 fsync，rename 在掉电后可能没生效（旧文件还在），
// 这是「写成功了但重启后回到旧状态」的经典坑。
func (s *fileStore) flush(df *diskFile) error {
	b, err := json.MarshalIndent(df, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: 序列化失败: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".schedules-*.tmp")
	if err != nil {
		return fmt.Errorf("schedule: 创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // rename 成功后这里是 no-op
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: 设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: 写临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("schedule: 落盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("schedule: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(name, s.path); err != nil {
		return fmt.Errorf("schedule: 替换 %s 失败: %w", s.path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ── Store 接口 ──────────────────────────────────────────────

func (s *fileStore) List(ctx context.Context, tenantID string) ([]Schedule, error) {
	out := make([]Schedule, 0, 8)
	err := s.read(ctx, func(df *diskFile) error {
		for _, it := range df.Items {
			if tenantID == "" || it.TenantID == tenantID {
				out = append(out, it)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 稳定顺序：先按创建时间，再按 ID（同一毫秒建的也不抖）。
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *fileStore) Get(ctx context.Context, id string) (Schedule, error) {
	var out Schedule
	err := s.read(ctx, func(df *diskFile) error {
		i := indexOf(df, id)
		if i < 0 {
			return fmt.Errorf("%w: 定时任务 %s", ErrNotFound, id)
		}
		out = df.Items[i]
		return nil
	})
	return out, err
}

func (s *fileStore) GetByHookToken(ctx context.Context, token string) (Schedule, error) {
	// 空 token 直接拒：否则一个没有 hook 的条目会被空串匹配到。
	if token == "" {
		return Schedule{}, fmt.Errorf("%w: hook token 为空", ErrNotFound)
	}
	var out Schedule
	found := false
	err := s.read(ctx, func(df *diskFile) error {
		for _, it := range df.Items {
			if it.HookToken != "" && subtleEqual(it.HookToken, token) {
				out, found = it, true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	if !found {
		return Schedule{}, fmt.Errorf("%w: hook token", ErrNotFound)
	}
	return out, nil
}

func (s *fileStore) Create(ctx context.Context, in Schedule) (Schedule, error) {
	if err := validate(in); err != nil {
		return Schedule{}, err
	}
	if in.Trigger == "" {
		in.Trigger = TriggerCron
	}
	now := time.Now().UTC()
	id, err := newID()
	if err != nil {
		return Schedule{}, err
	}
	in.ID = id
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	in.UpdatedAt = now

	switch in.Trigger {
	case TriggerCron:
		spec, err := parseCron(in.Cron, in.Timezone)
		if err != nil {
			return Schedule{}, err
		}
		in.HookToken = "" // cron 型不需要 token，清掉免得调用方塞了个假的
		in.NextRunAt = time.Time{}
		if in.Enabled {
			next, err := spec.Next(now)
			if err != nil {
				return Schedule{}, err
			}
			in.NextRunAt = next
		}
	case TriggerWebhook:
		tok, err := newHookToken()
		if err != nil {
			return Schedule{}, err
		}
		in.HookToken = tok
		in.NextRunAt = time.Time{} // webhook 没有「下次」，靠 token 触发
	default:
		return Schedule{}, fmt.Errorf("%w：trigger 只能是 cron 或 webhook", ErrInvalid)
	}

	var out Schedule
	err = s.update(ctx, func(df *diskFile) error {
		n := 0
		for _, it := range df.Items {
			if it.TenantID == in.TenantID {
				n++
			}
			if it.ID == in.ID {
				return fmt.Errorf("schedule: ID 撞车（%s），请重试", in.ID)
			}
		}
		if s.max > 0 && n >= s.max {
			return fmt.Errorf("%w（上限 %d 条）", ErrQuota, s.max)
		}
		df.Items = append(df.Items, in)
		out = in
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

func (s *fileStore) Update(ctx context.Context, in Schedule) error {
	if in.ID == "" {
		return fmt.Errorf("%w：缺少 id", ErrInvalid)
	}
	return s.update(ctx, func(df *diskFile) error {
		i := indexOf(df, in.ID)
		if i < 0 {
			return fmt.Errorf("%w: 定时任务 %s", ErrNotFound, in.ID)
		}
		prev := df.Items[i]
		// 归属与创建时间不可改，hook token 只能在建的时候生成。
		in.TenantID = prev.TenantID
		in.UserID = prev.UserID
		in.CreatedAt = prev.CreatedAt
		if in.Trigger == "" {
			in.Trigger = prev.Trigger
		}
		if in.Trigger == TriggerWebhook {
			in.HookToken = prev.HookToken
			if in.HookToken == "" {
				// cron 改成 webhook 的路径：补一个 token，否则这条永远触发不了。
				tok, err := newHookToken()
				if err != nil {
					return err
				}
				in.HookToken = tok
			}
			in.Cron = ""
			in.NextRunAt = time.Time{}
		}
		if in.Trigger == TriggerCron {
			in.HookToken = ""
			spec, err := parseCron(in.Cron, in.Timezone)
			if err != nil {
				return err
			}
			// 什么时候重算 NextRunAt：
			// 停用 → 清零；表达式/时区变了、或者本来就没有下次 → 从 now 重算。
			// 否则保留传进来的值——调度器推进 NextRunAt 走的就是 Update。
			switch {
			case !in.Enabled:
				in.NextRunAt = time.Time{}
			case in.Cron != prev.Cron || in.Timezone != prev.Timezone || in.NextRunAt.IsZero():
				next, err := spec.Next(time.Now())
				if err != nil {
					return err
				}
				in.NextRunAt = next
			}
		}
		in.UpdatedAt = time.Now().UTC()
		df.Items[i] = in
		return nil
	})
}

func (s *fileStore) Delete(ctx context.Context, tenantID, id string) error {
	return s.update(ctx, func(df *diskFile) error {
		i := indexOf(df, id)
		// 租户不匹配按「不存在」处理：跨租户删不能给出「存在但没权限」的信号。
		if i < 0 || (tenantID != "" && df.Items[i].TenantID != tenantID) {
			return fmt.Errorf("%w: 定时任务 %s", ErrNotFound, id)
		}
		df.Items = append(df.Items[:i], df.Items[i+1:]...)
		return nil
	})
}

func (s *fileStore) Due(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []Schedule
	err := s.read(ctx, func(df *diskFile) error {
		for _, it := range df.Items {
			if !it.Enabled || it.NextRunAt.IsZero() || it.NextRunAt.After(now) {
				continue
			}
			out = append(out, it)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NextRunAt.Before(out[j].NextRunAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ── 调度器内部用的原子操作 ───────────────────────────────────
//
// 这些放在 Store 接口之外：它们不是给 API 用的语义，
// 而是「读-改-写必须原子」的内部契约，暴露到接口上会诱使调用方误用。

// mutate 按 ID 原子地改一条记录，返回改完的副本。
func (s *fileStore) mutate(ctx context.Context, id string, fn func(*Schedule) error) (Schedule, error) {
	var out Schedule
	err := s.update(ctx, func(df *diskFile) error {
		i := indexOf(df, id)
		if i < 0 {
			return fmt.Errorf("%w: 定时任务 %s", ErrNotFound, id)
		}
		it := df.Items[i]
		if err := fn(&it); err != nil {
			return err
		}
		it.UpdatedAt = time.Now().UTC()
		df.Items[i] = it
		out = it
		return nil
	})
	return out, err
}

// claim 是触发前的乐观锁：只有当磁盘上的 NextRunAt 仍等于 expect 时，
// 才把它改成 next 并返回 true。多副本同时扫到同一条时，只有一个能成功。
//
// 顺序很关键：先落盘再触发。反过来的话「创建了 run 但没来得及改 NextRunAt」
// 会在下一次 Tick 重复触发。
func (s *fileStore) claim(ctx context.Context, id string, expect, next time.Time) (bool, error) {
	ok := false
	_, err := s.mutate(ctx, id, func(it *Schedule) error {
		if !it.NextRunAt.Equal(expect) {
			return errStale
		}
		it.NextRunAt = next
		ok = true
		return nil
	})
	if errors.Is(err, errStale) {
		return false, nil
	}
	return ok, err
}

// errStale 表示乐观锁没抢到（别人已经推进过 NextRunAt）。不落盘。
var errStale = errors.New("schedule: 已被其他实例抢到")

// ── 小工具 ──────────────────────────────────────────────────

func indexOf(df *diskFile, id string) int {
	for i := range df.Items {
		if df.Items[i].ID == id {
			return i
		}
	}
	return -1
}

func validate(s Schedule) error {
	if s.TenantID == "" {
		return fmt.Errorf("%w：缺少 tenant_id", ErrInvalid)
	}
	if s.SessionID == "" {
		return fmt.Errorf("%w：缺少 session_id", ErrInvalid)
	}
	if s.Prompt == "" {
		return fmt.Errorf("%w：prompt 不能为空", ErrInvalid)
	}
	return nil
}

// newID 是 16 位十六进制（8 字节随机）。够用且短，日志里好认。
func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("schedule: 生成 ID 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// newHookToken 是 32 字节随机，base64url 无填充（43 字符）。
// 无填充是为了直接塞 URL 里不出现 %3D。
func newHookToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("schedule: 生成 hook token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// subtleEqual 常量时间比较，避免按字节短路泄露 token 前缀。
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
