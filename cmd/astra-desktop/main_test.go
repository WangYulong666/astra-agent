package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// probeEnv 把影响 dshStatus 裁决的环境变量清空（ASTRA_AGENT / PATH），
// 让用例只由传入的路径参数决定结果。t.Setenv 会在用例结束后自动还原。
func probeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ASTRA_AGENT", "")
	t.Setenv("PATH", "")
}

func mkBin(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh", "lib")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(p, "bin.js")
	if err := os.WriteFile(bin, []byte("// fake dsh"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// 用例 1：home 与 bundle 都不存在 → 不可用（应降级）。
func TestDSHStatus_NotFound(t *testing.T) {
	probeEnv(t)
	path, fromBundle, ok := dshStatus(
		filepath.Join(t.TempDir(), "nope", "bin.js"),
		filepath.Join(t.TempDir(), "nope", "bin.js"))
	if ok {
		t.Fatalf("不存在时应不可用，却返回 ok path=%q", path)
	}
	if path != "" || fromBundle {
		t.Fatalf("不可用时 path/fromBundle 应为零值，得到 path=%q fromBundle=%v", path, fromBundle)
	}
}

// 用例 2：home 存在 → 可用且不算 bundle。
func TestDSHStatus_HomeExists(t *testing.T) {
	probeEnv(t)
	home := mkBin(t, t.TempDir())
	path, fromBundle, ok := dshStatus(home, filepath.Join(t.TempDir(), "bundle-bin.js"))
	if !ok || fromBundle || path != home {
		t.Fatalf("home 存在应命中 home：ok=%v fromBundle=%v path=%q", ok, fromBundle, path)
	}
}

// 用例 3：home 缺失但 bundle 存在 → 可用且标记来自 bundle（触发 ASTRA_AGENT 重定向）。
func TestDSHStatus_BundleFallback(t *testing.T) {
	probeEnv(t)
	bundle := mkBin(t, t.TempDir())
	path, fromBundle, ok := dshStatus(filepath.Join(t.TempDir(), "nope", "bin.js"), bundle)
	if !ok || !fromBundle || path != bundle {
		t.Fatalf("仅 bundle 存在应命中 bundle：ok=%v fromBundle=%v path=%q", ok, fromBundle, path)
	}
}

// 用例 4：两处都在 → home 优先（与 DefaultAgentCommand 的解析顺序一致，不做无谓重定向）。
func TestDSHStatus_HomeWins(t *testing.T) {
	probeEnv(t)
	home := mkBin(t, t.TempDir())
	bundle := mkBin(t, t.TempDir())
	path, fromBundle, ok := dshStatus(home, bundle)
	if !ok || fromBundle || path != home {
		t.Fatalf("两处都在应选 home：ok=%v fromBundle=%v path=%q", ok, fromBundle, path)
	}
}

// 用例 5：目录存在但不是可执行文件（如空目录）不算可用。
func TestDSHStatus_DirectoryNotOK(t *testing.T) {
	probeEnv(t)
	dir := t.TempDir()
	_, _, ok := dshStatus(dir, dir)
	if ok {
		t.Fatal("目录不应被判为可用 dsh 入口")
	}
}

// 用例 6：备份 rsync 排除 sandboxes 且产物是合法 zip。
func TestBackupDataTo(t *testing.T) {
	src := t.TempDir()
	for _, f := range []string{"sqlite.db", "sessions/a.json"} {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "sandboxes", "sb-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sandboxes", "sb-1", "big.bin"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "Astra-backup-test.zip")
	if err := backupDataTo(src, dst); err != nil {
		t.Fatalf("备份失败: %v", err)
	}
	if st, err := os.Stat(dst); err != nil || st.Size() == 0 {
		t.Fatalf("产物异常: size=%d err=%v", st.Size(), err)
	}
	// 解开验证：sandboxes 不在包里，普通文件在。
	unpack := t.TempDir()
	if b, err := exec.Command("ditto", "-x", "-k", dst, unpack).CombinedOutput(); err != nil {
		t.Fatalf("解包失败: %v: %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(unpack, "data", "sqlite.db")); err != nil {
		t.Fatalf("sqlite.db 应在备份内: %v", err)
	}
	if _, err := os.Stat(filepath.Join(unpack, "data", "sandboxes")); !os.IsNotExist(err) {
		t.Fatalf("sandboxes 应被排除，stat err=%v", err)
	}
}
