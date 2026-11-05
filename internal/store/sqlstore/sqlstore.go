// Package sqlstore 是 store.Store 与 store.AdminStore 的 SQL 实现：
// 同一份语句同时服务 SQLite（单机、本地开发）与 PostgreSQL（多副本生产）。
//
// 取舍见 docs/平台化设计-认证与治理.md 的 D2：语句里一律写 ? 占位符，
// 执行前用 rebind 换成 PG 的 $n；类型差异只体现在两份 embed 的 DDL 上。
// 这样业务逻辑和测试只有一份，代价是每条语句都得保证两种方言都支持——
// 所以这里刻意不用方言专有的写法（窗口函数、jsonb 操作符、UPDATE ... FROM 之类），
// upsert 只用两边都支持的 ON CONFLICT ... DO UPDATE。
package sqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" 驱动
	sqlite "modernc.org/sqlite"        // 注册 "sqlite" 驱动；同时用于识别唯一约束冲突
)

const (
	dialectSQLite   = "sqlite"
	dialectPostgres = "postgres"
)

// sqliteTimeLayout 是 SQLite 里时间列的落库格式。
//
// 为什么不用 time.RFC3339Nano 直接格式化：RFC3339Nano 会裁掉小数末尾的 0，
// 得到变长字符串。SQLite 没有时间类型，created_at 的区间过滤和 ORDER BY
// 全靠字符串比较，而 "…:00Z" > "…:00.5Z" 这种字典序会把同一秒内的事件排反。
// 固定九位小数后，字典序与时间序一致。读取仍用 RFC3339Nano 解析（能兼容两种）。
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// Store 同时实现运行时存储与治理层存储。
//
// 用 database/sql 的连接池并把并发正确性交给数据库：Append 的 seq 分配
// 走单条 UPDATE ... RETURNING，靠数据库的行锁串行化，进程内不加锁——
// 否则多副本部署时进程内的锁保护不了任何人。
type Store struct {
	db      *sql.DB
	dialect string
}

var (
	_ store.Store      = (*Store)(nil)
	_ store.AdminStore = (*Store)(nil)
)

// Open 按 DSN 前缀选驱动并完成建表/迁移。
//
// 前缀规则（docs/平台化设计 的 D2）：
//
//	sqlite://path  sqlite:path  → modernc.org/sqlite（纯 Go，无 cgo）
//	postgres://... postgresql:// → github.com/jackc/pgx/v5/stdlib
func Open(dsn string) (*Store, error) {
	var (
		s   *Store
		err error
	)
	switch {
	case strings.HasPrefix(dsn, "sqlite://"), strings.HasPrefix(dsn, "sqlite:"):
		s, err = openSQLite(dsn)
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		s, err = openPostgres(dsn)
	default:
		return nil, fmt.Errorf("sqlstore: 不支持的 DSN %q：需以 sqlite:// 、sqlite: 、postgres:// 或 postgresql:// 开头", dsn)
	}
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		_ = s.db.Close()
		return nil, fmt.Errorf("sqlstore: 连接数据库失败: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		_ = s.db.Close()
		return nil, err
	}
	return s, nil
}

func openSQLite(raw string) (*Store, error) {
	// sqlite:///abs/path.db → 去掉 "sqlite:" 得 "///abs/path.db"，再吃掉 "//" 得 "/abs/path.db"；
	// sqlite://rel.db → "rel.db"；sqlite:file:x.db?cache=shared → 原样透传给驱动。
	path := strings.TrimPrefix(strings.TrimPrefix(raw, "sqlite:"), "//")
	if path == "" {
		return nil, errors.New("sqlstore: sqlite DSN 缺少文件路径")
	}

	// 内存库的 DSN 是 ":memory:" 或 "file:...?mode=memory"，拼参数会让驱动解析失败，
	// 而内存库也没有跨连接并发的意义，直接跳过。
	if !isMemoryDSN(path) {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		// busy_timeout：并发写时让 SQLite 排队等待而不是立刻回 SQLITE_BUSY；
		// journal_mode=WAL：读不被写阻塞，否则并发 Append 期间 Events 会卡住；
		// foreign_keys：SQLite 默认关闭外键，不开的话删会话不会级联删 run/事件；
		// _txlock=immediate：事务开始就拿写锁。默认的 deferred 在两个写事务
		// 同时「读后升级为写」时会直接返回 SQLITE_BUSY 且不重试，Append 会随机失败。
		path += sep + "_pragma=busy_timeout(10000)" +
			"&_pragma=journal_mode(WAL)" +
			"&_pragma=foreign_keys(1)" +
			"&_txlock=immediate"
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: 打开 sqlite 失败: %w", err)
	}
	// 单连接：SQLite 本来就只有单写者，连接池开到多个只会让并发写退化成
	// 抢锁 + busy_timeout 空转（实测 50 并发 Append 从 ~70ms 恶化到 ~2.6s），
	// 而换来的读并发在这个规模上收益可以忽略。
	// 注意：这条约束要求任何方法内部不能在持有事务时再调用本 Store 的其它方法。
	db.SetMaxOpenConns(1)
	return &Store{db: db, dialect: dialectSQLite}, nil
}

func isMemoryDSN(path string) bool {
	return path == ":memory:" || strings.Contains(path, "mode=memory")
}

func openPostgres(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: 打开 postgres 失败: %w", err)
	}
	// 多副本时每个副本都连一个池，这里的上限只是单副本的自我保护。
	// 不设 ConnMaxLifetime 的话，PG 侧重启或连接被中间设备掐断后，
	// 池里会长期留着死连接，只能靠 Ping 时才暴露。
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Store{db: db, dialect: dialectPostgres}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ── 迁移 ─────────────────────────────────────────────────────

//go:embed schema_sqlite.sql
var schemaSQLite string

//go:embed schema_pg.sql
var schemaPG string

// migrationsTableDDL 单独建而不放进 DDL 文件：它必须先于任何 DDL 存在，
// 而两种方言的时间列类型不同，塞进两份文件等于把同一张表定义两遍。
const migrationsTableDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    TEXT PRIMARY KEY,
	applied_at %s NOT NULL
)`

// migrations 按数组顺序执行，版本号只增不改。
// 新增结构变更时往末尾追加一条，不要改已有条目——已部署的库不会重跑它。
//
// fn 字段用于「无法用一段静态 DDL 表达」的变更：它拿 Store 自己判断现状再动手，
// 必须写成可重跑的（执行成功但版本号尚未记录时，下次启动会再来一遍）。
var migrations = []struct {
	version string
	sqlite  string
	pg      string
	fn      func(ctx context.Context, s *Store) error
}{
	{version: "0001_init", sqlite: schemaSQLite, pg: schemaPG},
	{version: "0002_approvals", sqlite: approvalsUpgradeSQLite, pg: approvalsUpgradePG},
	{version: "0003_user_token_valid_after", fn: migrateUserTokenValidAfter},
}

// 0002 的 DDL 与 schema_*.sql 里的 approvals 表重复一份，是刻意的：
// 0001 已经在老库上记过版本号，改 schema 文件不会让它重跑，只有新版本号能补表。
// 全新库上 0002 因为 IF NOT EXISTS 变成空操作，不会和 0001 冲突。
const approvalsUpgradeSQLite = `
CREATE TABLE IF NOT EXISTS approvals (
    run_id       TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    tool_call_id TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    resolved_at  TEXT,
    resolved_by  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, tool_call_id)
);
CREATE INDEX IF NOT EXISTS idx_approvals_status_created ON approvals (status, created_at);
`

const approvalsUpgradePG = `
CREATE TABLE IF NOT EXISTS approvals (
    run_id       text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    tool_call_id text NOT NULL,
    title        text NOT NULL DEFAULT '',
    kind         text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'pending',
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    resolved_at  timestamptz,
    resolved_by  text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, tool_call_id)
);
CREATE INDEX IF NOT EXISTS idx_approvals_status_created ON approvals (status, created_at);
`

// migrateUserTokenValidAfter 给 users 表补 token_valid_after 列（0003）。
//
// 不能写成静态 DDL：SQLite 的 ALTER TABLE ADD COLUMN 没有 IF NOT EXISTS，
// 而全新库在 0001 里已经带上了这一列，无条件 ALTER 会在新库上直接报错。
// 所以先查列存在性再动手，天然可重跑。
func migrateUserTokenValidAfter(ctx context.Context, s *Store) error {
	if s.dialect == dialectPostgres {
		_, err := s.db.ExecContext(ctx,
			`ALTER TABLE users ADD COLUMN IF NOT EXISTS token_valid_after timestamptz`)
		if err != nil {
			return fmt.Errorf("sqlstore: 迁移 0003 执行失败: %w", err)
		}
		return nil
	}

	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		return fmt.Errorf("sqlstore: 迁移 0003 查询列失败: %w", err)
	}
	defer rows.Close()
	has := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("sqlstore: 迁移 0003 读取列名失败: %w", err)
		}
		if name == "token_valid_after" {
			has = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlstore: 迁移 0003 查询列失败: %w", err)
	}
	if has {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE users ADD COLUMN token_valid_after TEXT`); err != nil {
		return fmt.Errorf("sqlstore: 迁移 0003 执行失败: %w", err)
	}
	return nil
}

// migrate 按版本号顺序补齐未应用的 DDL，可重复启动。
//
// 每条迁移的 DDL 与版本行写在同一个事务里：PG 支持事务性 DDL，
// 中途失败会整体回滚，不会留下「表建了一半但版本已记」的半成品。
func (s *Store) migrate(ctx context.Context) error {
	appliedType := "TEXT"
	if s.dialect == dialectPostgres {
		appliedType = "TIMESTAMPTZ"
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(migrationsTableDDL, appliedType)); err != nil {
		return fmt.Errorf("sqlstore: 建 schema_migrations 失败: %w", err)
	}

	done := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("sqlstore: 读迁移记录失败: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("sqlstore: 读迁移记录失败: %w", err)
		}
		done[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sqlstore: 读迁移记录失败: %w", err)
	}
	rows.Close()

	for _, m := range migrations {
		if done[m.version] {
			continue
		}
		if m.fn != nil {
			// fn 型迁移自带幂等（见 migrateUserTokenValidAfter）。它必须先于
			// 事务执行：SQLite 连接池只有一条连接，事务未提交时 fn 里的任何
			// 查询都会等锁直到超时。版本号单独记，fn 成功但记录失败时，
			// 下次启动重跑一遍空操作，仍然安全。
			if err := m.fn(ctx, s); err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx,
				s.rebind("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)"),
				m.version, s.timeVal(time.Now().UTC())); err != nil {
				return fmt.Errorf("sqlstore: 记录迁移 %s 失败: %w", m.version, err)
			}
			continue
		}
		script := m.sqlite
		if s.dialect == dialectPostgres {
			script = m.pg
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlstore: 迁移 %s 开启事务失败: %w", m.version, err)
		}
		for _, stmt := range splitStatements(script) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("sqlstore: 迁移 %s 执行失败: %w", m.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			s.rebind("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)"),
			m.version, s.timeVal(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlstore: 记录迁移 %s 失败: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlstore: 迁移 %s 提交失败: %w", m.version, err)
		}
	}
	return nil
}

// splitStatements 按分号切分 DDL 脚本，并去掉 -- 行注释。
//
// 为什么不把整段脚本一次 Exec：pgx 在无参数时走简单协议确实能一次执行多语句，
// 但 modernc/sqlite 的行为随版本变化，逐条执行对两边都确定。
// DDL 里没有字符串字面量，所以「先剥注释再按分号切」不会切错。
func splitStatements(script string) []string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	out := make([]string, 0, 16)
	for _, stmt := range strings.Split(b.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ── 占位符与时间 ─────────────────────────────────────────────

// rebind 把 ? 换成 PG 的 $1..$n，SQLite 原样返回——一份语句两种方言都能跑。
// 只跳过单引号字符串内的 ?：查询文本是我们自己写的，但仍不该把 '?' 字面量当占位符。
func (s *Store) rebind(q string) string {
	if s.dialect != dialectPostgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'':
			// 语句里没有转义单引号（''）的用法，简单翻转即可
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// timeVal 把时间转成当前方言的列值。零值存 NULL，读出来还是零值。
//
// 统一 .UTC()：库里存的时间列一律 UTC，本地时区只存在于调用方上下文，
// 否则同一个 API 在 SQLite（字符串原样存）和 PG（驱动带时区）上会返回不同偏移。
func (s *Store) timeVal(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	if s.dialect == dialectSQLite {
		return t.Format(sqliteTimeLayout)
	}
	return t
}

// dbTime 统一扫描时间列：SQLite 给 string，PG 的 timestamptz 给 time.Time，
// 两种都要能落到 time.Time 并转 UTC。
type dbTime struct{ t time.Time }

func (d *dbTime) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		d.t = time.Time{}
	case time.Time:
		d.t = x.UTC()
	case string:
		return d.parse(x)
	case []byte:
		return d.parse(string(x))
	default:
		return fmt.Errorf("sqlstore: 时间列类型不识别 %T", v)
	}
	return nil
}

func (d *dbTime) parse(s string) error {
	if s == "" {
		d.t = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// 兼容手工灌入的 "2006-01-02 15:04:05" 之类历史数据
		if t, err = time.Parse("2006-01-02 15:04:05", s); err != nil {
			return fmt.Errorf("sqlstore: 时间 %q 解析失败: %w", s, err)
		}
	}
	d.t = t.UTC()
	return nil
}

// ── 错误映射 ─────────────────────────────────────────────────

// mapErr 把驱动错误收敛成 store 包的哨兵错误。
//
// 两种数据库的错误码完全不同：PG 用 SQLSTATE 23505，SQLite 用扩展码 2067/1555
// 且文本为 "UNIQUE constraint failed"。上层只认 store.ErrDuplicate / ErrNotFound，
// 所以差异必须在这里吃掉。
func (s *Store) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %v", store.ErrDuplicate, err)
	}
	return err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505" // unique_violation
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		// 2067 = SQLITE_CONSTRAINT_UNIQUE，1555 = SQLITE_CONSTRAINT_PRIMARYKEY
		return liteErr.Code() == 2067 || liteErr.Code() == 1555
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE") ||
		strings.Contains(msg, "duplicate key value violates unique constraint")
}

// ── 小工具 ───────────────────────────────────────────────────

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(q), args...)
	return res, s.mapErr(err)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	return rows, s.mapErr(err)
}

// queryRowScan 是绕不开的一层包装：*sql.Row 的 Scan 只能在调用处做，
// 但 ErrNoRows → ErrNotFound 的映射不想在每个方法里重复一遍。
func (s *Store) queryRowScan(ctx context.Context, q string, args []any, dest ...any) error {
	return s.mapErr(s.db.QueryRowContext(ctx, s.rebind(q), args...).Scan(dest...))
}

// newID 与 internal/runner 里的 newID 保持一致：12 字节随机数的 hex。
// 只在调用方没给 ID 时兜底（用量、审计这类高频写入），
// 其余实体的 ID 由上层生成，存储层不擅自改写。
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// scanner 抽出来是为了让同一个行扫描函数能被 *sql.Row 和 *sql.Rows 复用。
type scanner interface{ Scan(dest ...any) error }
