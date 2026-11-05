-- SQLite 版 DDL。与 schema_pg.sql 逐列对应，差异只在类型：
--   * 时间列：TEXT 存定长小数的 RFC3339（见 Go 里的 sqliteTimeLayout）
--   * 计数/token：INTEGER（SQLite 的 INTEGER 本来就是 64 位）
--   * PG 的 timestamptz / bigint 在这里退化成 TEXT / INTEGER
--
-- schema_migrations 不在这份文件里：两种方言的类型不同，且它必须先于任何
-- DDL 存在，所以由 Go 代码按方言建（见 sqlstore.go 的 migrationsTableDDL）。
--
-- 为什么没有把 DDL 拆成一个版本一个文件：本期只有一个初始版本，
-- 版本号由 migrations 变量登记，DDL 文件只负责「当前全量结构」。

CREATE TABLE IF NOT EXISTS tenants (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL DEFAULT '',
    slug                TEXT NOT NULL UNIQUE,          -- 登录用短名，全局唯一
    status              TEXT NOT NULL DEFAULT 'active',
    monthly_token_limit INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email         TEXT NOT NULL,                       -- 写入前统一转小写
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'member',
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TEXT NOT NULL,
    last_login_at TEXT,
    -- 整体失效凭据的时间戳：iat 不严格晚于它的 token 一律拒绝（改密码时更新）。
    -- 老库升级由 0003_user_token_valid_after 迁移补列（见 sqlstore.go）。
    token_valid_after TEXT,
    -- 邮箱只在租户内唯一：同一个人可以是多个租户的成员
    UNIQUE (tenant_id, email)
);

-- 唯一约束本身就带索引，这里只补 tenant 维度的列表查询
CREATE INDEX IF NOT EXISTS idx_users_tenant ON users (tenant_id, created_at);

CREATE TABLE IF NOT EXISTS apikeys (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    prefix       TEXT NOT NULL DEFAULT '',             -- 列表展示用，不是凭据
    key_hash     TEXT NOT NULL UNIQUE,                 -- sha256(明文)，明文不落库
    created_at   TEXT NOT NULL,
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_apikeys_tenant ON apikeys (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
    id               TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL DEFAULT '',
    user_id          TEXT NOT NULL DEFAULT '',
    sandbox_id       TEXT NOT NULL DEFAULT '',
    agent_session_id TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL DEFAULT '',
    cwd              TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'active',
    created_at       TEXT NOT NULL,
    last_active_at   TEXT NOT NULL
);

-- API 层按租户列会话，且默认按活跃时间倒序
CREATE INDEX IF NOT EXISTS idx_sessions_tenant_active ON sessions (tenant_id, last_active_at DESC);
-- member 角色还要再按 user 过滤，单独给一条（tenant_id 的选择性不足以覆盖它）
CREATE INDEX IF NOT EXISTS idx_sessions_user_active ON sessions (tenant_id, user_id, last_active_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_created ON sessions (created_at DESC);

CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    status      TEXT NOT NULL DEFAULT 'queued',
    input       TEXT NOT NULL DEFAULT '',
    stop_reason TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    -- next_seq 是「下一个待分配」的 seq，Append 靠 UPDATE ... RETURNING 原子递增
    next_seq    INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    started_at  TEXT,
    ended_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_session_created ON runs (session_id, created_at);

CREATE TABLE IF NOT EXISTS run_events (
    run_id     TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    type       TEXT NOT NULL,
    -- payload 存 JSON 文本而不是 SQLite 的 JSON 函数结果：两种后端扫描方式一致，
    -- 也不用在 Go 侧为 jsonb 单独写解码分支
    payload    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    -- 主键即「同一 run 内 seq 唯一」，并发 Append 分配重号时这里会直接报错
    PRIMARY KEY (run_id, seq)
);

-- 人工审批。老库升级由 0002_approvals 迁移补建（见 sqlstore.go），
-- 这里保留全量结构给全新库；两处 DDL 必须保持一致。
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
    -- 一条请求的身份就是 (run, toolCall)：不要另造 id
    PRIMARY KEY (run_id, tool_call_id)
);

-- 启动时的「把所有 pending 标 expired」和按 run 查待审批都走这里
CREATE INDEX IF NOT EXISTS idx_approvals_status_created ON approvals (status, created_at);

CREATE TABLE IF NOT EXISTS usage (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL,
    user_id        TEXT NOT NULL DEFAULT '',
    session_id     TEXT NOT NULL DEFAULT '',
    run_id         TEXT NOT NULL DEFAULT '',
    provider       TEXT NOT NULL DEFAULT '',
    model          TEXT NOT NULL DEFAULT '',
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    cost_micro_usd INTEGER NOT NULL DEFAULT 0,
    source         TEXT NOT NULL DEFAULT 'gateway',
    created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_tenant_created ON usage (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_user_created ON usage (user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage (session_id);

CREATE TABLE IF NOT EXISTS audit_logs (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL DEFAULT '',
    user_id     TEXT NOT NULL DEFAULT '',
    actor_email TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    detail      TEXT,
    ip          TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_tenant_created ON audit_logs (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs (action, created_at DESC);
