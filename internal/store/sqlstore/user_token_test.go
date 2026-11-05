package sqlstore

// users 表 token_valid_after 列的测试：round-trip + 老库升级。
//
// 老库升级测试是这一组里最有价值的：它模拟「0003 之前部署的库」——
// schema_migrations 里已记到 0002、users 表没有新列——验证 Open 能补上列，
// 这条路径靠普通 round-trip 测试根本覆盖不到。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/store"
)

// TestUserTokenValidAfterRoundTrip 字段要能写进去、读出来，零值保持零值。
func TestUserTokenValidAfterRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 老用户（没有失效时间）不带这个字段也能正常读写。
	old := store.User{
		ID: "u0", TenantID: "t1", Email: "old@x.test", Role: store.RoleMember,
		Status: store.StatusActive, CreatedAt: time.Now().UTC(),
	}
	// 需要 tenants 表有对应行（外键约束）。
	if err := s.CreateTenant(ctx, store.Tenant{ID: "t1", Name: "T", Slug: "t", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.CreateUser(ctx, old); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	got, err := s.GetUser(ctx, "u0")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !got.TokenValidAfter.IsZero() {
		t.Fatalf("未设置时应为零值，实际 %v", got.TokenValidAfter)
	}

	// 设置后 round-trip，纳秒精度允许被秒级存储截断，比较到秒。
	va := time.Now().UTC().Truncate(time.Second).Add(3 * time.Second)
	old.TokenValidAfter = va
	if err := s.UpdateUser(ctx, old); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got, err = s.GetUser(ctx, "u0")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.TokenValidAfter.Truncate(time.Second) != va {
		t.Fatalf("token_valid_after 丢失: 期望 %v，实际 %v", va, got.TokenValidAfter)
	}

	// 清零也要能写回去（管理员解除限制的场景）。
	old.TokenValidAfter = time.Time{}
	if err := s.UpdateUser(ctx, old); err != nil {
		t.Fatalf("UpdateUser(清零): %v", err)
	}
	if got, _ = s.GetUser(ctx, "u0"); !got.TokenValidAfter.IsZero() {
		t.Fatalf("清零后应为零值，实际 %v", got.TokenValidAfter)
	}
}

// TestMigrateUpgradeFromPre0003 模拟 0003 之前的老库：迁移记录停在 0002、
// users 表没有 token_valid_after 列。Open 必须补列且数据无损。
func TestMigrateUpgradeFromPre0003(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")

	// 手工搭一个「老库」：老 users 表 + 记到 0002 的迁移历史。
	// 表结构抄自 0001 版 schema（不含新列），这是测试夹具，不是现行 DDL。
	s, err := openSQLite("sqlite://" + dbPath)
	if err != nil {
		t.Fatalf("打开老库失败: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE tenants (
			id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
			slug TEXT NOT NULL UNIQUE, status TEXT NOT NULL DEFAULT 'active',
			monthly_token_limit INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL)`,
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
			email TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT 'member',
			status TEXT NOT NULL DEFAULT 'active', created_at TEXT NOT NULL,
			last_login_at TEXT, UNIQUE (tenant_id, email))`,
		`INSERT INTO tenants (id, name, slug, status, monthly_token_limit, created_at)
			VALUES ('t1', 'T', 't', 'active', 0, '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO users (id, tenant_id, email, display_name, password_hash, role, status, created_at)
			VALUES ('u1', 't1', 'a@x.test', 'A', 'h', 'owner', 'active', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES ('0001_init', '2026-01-01T00:00:00.000000000Z')`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES ('0002_approvals', '2026-01-01T00:00:00.000000000Z')`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("搭老库失败: %v", err)
		}
	}
	_ = s.Close()

	// 重新 Open：迁移必须补上列，且老数据原样可读。
	fresh, err := Open("sqlite://" + dbPath)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer fresh.Close()

	u, err := fresh.GetUser(ctx, "u1")
	if err != nil {
		t.Fatalf("升级后读用户失败: %v", err)
	}
	if u.Email != "a@x.test" || !u.TokenValidAfter.IsZero() {
		t.Fatalf("老数据应无损且新列为零值: %+v", u)
	}

	// 新列立即可写。
	u.TokenValidAfter = time.Now().UTC().Truncate(time.Second)
	if err := fresh.UpdateUser(ctx, u); err != nil {
		t.Fatalf("升级后写 token_valid_after 失败: %v", err)
	}

	// 再开一次：迁移幂等，不会因为列已存在而报错。
	again, err := Open("sqlite://" + dbPath)
	if err != nil {
		t.Fatalf("重复 Open 失败: %v", err)
	}
	defer again.Close()
}
