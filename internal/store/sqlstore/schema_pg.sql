-- PostgreSQL 版 DDL。与 schema_sqlite.sql 逐列对应，差异只在类型：
--   * 时间列：timestamptz（应用侧统一写 UTC，读出后也转 UTC）
--   * 计数/token/seq：bigint
--   * payload / detail 仍是 text：与 SQLite 保持同一套扫描逻辑，
--     换 jsonb 会引入驱动侧的额外解码分支，却没有查询需求需要它

CREATE TABLE IF NOT EXISTS tenants (
    id                  text PRIMARY KEY,
    name                text NOT NULL DEFAULT '',
    slug                text NOT NULL UNIQUE,
    status              text NOT NULL DEFAULT 'active',
    monthly_token_limit bigint NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    email         text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    password_hash text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'member',
    status        text NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL,
    last_login_at timestamptz,
    -- 整体失效凭据的时间戳：iat 不严格晚于它的 token 一律拒绝（改密码时更新）。
    -- 老库升级由 0003_user_token_valid_after 迁移补列（见 sqlstore.go）。
    token_valid_after timestamptz,
    UNIQUE (tenant_id, email)
);

CREATE INDEX IF NOT EXISTS idx_users_tenant ON users (tenant_id, created_at);

CREATE TABLE IF NOT EXISTS apikeys (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id      text NOT NULL DEFAULT '',
    name         text NOT NULL DEFAULT '',
    prefix       text NOT NULL DEFAULT '',
    key_hash     text NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX IF NOT EXISTS idx_apikeys_tenant ON apikeys (tenant_id, created_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL DEFAULT '',
    user_id          text NOT NULL DEFAULT '',
    sandbox_id       text NOT NULL DEFAULT '',
    agent_session_id text NOT NULL DEFAULT '',
    title            text NOT NULL DEFAULT '',
    cwd              text NOT NULL DEFAULT '',
    status           text NOT NULL DEFAULT 'active',
    created_at       timestamptz NOT NULL,
    last_active_at   timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_tenant_active ON sessions (tenant_id, last_active_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_user_active ON sessions (tenant_id, user_id, last_active_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_created ON sessions (created_at DESC);

CREATE TABLE IF NOT EXISTS runs (
    id          text PRIMARY KEY,
    session_id  text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    status      text NOT NULL DEFAULT 'queued',
    input       text NOT NULL DEFAULT '',
    stop_reason text NOT NULL DEFAULT '',
    error       text NOT NULL DEFAULT '',
    next_seq    bigint NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL,
    started_at  timestamptz,
    ended_at    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_runs_session_created ON runs (session_id, created_at);

CREATE TABLE IF NOT EXISTS run_events (
    run_id     text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq        bigint NOT NULL,
    type       text NOT NULL,
    payload    text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- 人工审批。老库升级由 0002_approvals 迁移补建（见 sqlstore.go），
-- 这里保留全量结构给全新库；两处 DDL 必须保持一致。
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

CREATE TABLE IF NOT EXISTS usage (
    id             text PRIMARY KEY,
    tenant_id      text NOT NULL,
    user_id        text NOT NULL DEFAULT '',
    session_id     text NOT NULL DEFAULT '',
    run_id         text NOT NULL DEFAULT '',
    provider       text NOT NULL DEFAULT '',
    model          text NOT NULL DEFAULT '',
    input_tokens   bigint NOT NULL DEFAULT 0,
    output_tokens  bigint NOT NULL DEFAULT 0,
    cost_micro_usd bigint NOT NULL DEFAULT 0,
    source         text NOT NULL DEFAULT 'gateway',
    created_at     timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_usage_tenant_created ON usage (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_user_created ON usage (user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_session ON usage (session_id);

CREATE TABLE IF NOT EXISTS audit_logs (
    id          text PRIMARY KEY,
    tenant_id   text NOT NULL DEFAULT '',
    user_id     text NOT NULL DEFAULT '',
    actor_email text NOT NULL DEFAULT '',
    action      text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id   text NOT NULL DEFAULT '',
    detail      text,
    ip          text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_tenant_created ON audit_logs (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_logs (action, created_at DESC);
