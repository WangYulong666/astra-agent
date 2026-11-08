package app

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func appTestLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// writeExecutable 放一个带可执行位的文件，供 ensureCLIBin 的候选路径使用。
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCLIBinEnvWins(t *testing.T) {
	dir := t.TempDir()
	exeDir := t.TempDir()
	writeExecutable(t, filepath.Join(exeDir, "astra-cli"), "from-exe")
	// env 显式指定时优先，且不该触发任何拷贝。
	got := ensureCLIBinFrom(Config{DataDir: dir}, appTestLogger(), "/opt/custom/astra-cli", exeDir)
	if got != "/opt/custom/astra-cli" {
		t.Fatalf("ASTRA_CLI_BIN 应优先返回, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "astra-cli")); !os.IsNotExist(err) {
		t.Fatalf("env 命中时不应拷贝: %v", err)
	}
}

func TestEnsureCLIBinReturnsExistingDataDirBin(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bin", "astra-cli")
	writeExecutable(t, dst, "already-there")
	exeDir := t.TempDir()
	writeExecutable(t, filepath.Join(exeDir, "astra-cli"), "different")

	got := ensureCLIBinFrom(Config{DataDir: dir}, appTestLogger(), "", exeDir)
	if got != dst {
		t.Fatalf("应直接返回 <dataDir>/bin/astra-cli, got %q", got)
	}
	b, _ := os.ReadFile(dst)
	if string(b) != "already-there" {
		t.Fatalf("已有的二进制不该被覆盖: %q", b)
	}
}

func TestEnsureCLIBinCopiesFromExeDir(t *testing.T) {
	dir := t.TempDir()
	exeDir := t.TempDir()
	writeExecutable(t, filepath.Join(exeDir, "astra-cli"), "CLI-BINARY-CONTENT")

	got := ensureCLIBinFrom(Config{DataDir: dir}, appTestLogger(), "", exeDir)
	dst := filepath.Join(dir, "bin", "astra-cli")
	if got != dst {
		t.Fatalf("拷贝后应返回 %q, got %q", dst, got)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("拷贝产物读不到: %v", err)
	}
	if string(b) != "CLI-BINARY-CONTENT" {
		t.Fatalf("拷贝内容 = %q, want CLI-BINARY-CONTENT", b)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("拷贝产物权限 = %o, want 755", fi.Mode().Perm())
		}
		di, err := os.Stat(filepath.Dir(dst))
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("bin 目录权限 = %o, want 700", di.Mode().Perm())
		}
	}
}

func TestEnsureCLIBinDegradesWhenNotFound(t *testing.T) {
	dir := t.TempDir()
	exeDir := t.TempDir() // 空目录
	got := ensureCLIBinFrom(Config{DataDir: dir}, appTestLogger(), "", exeDir)
	if got != "" {
		t.Fatalf("找不到应返回空串（requires 标 unavailable 降级）, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "astra-cli")); !os.IsNotExist(err) {
		t.Fatalf("降级时不应产出文件: %v", err)
	}
}

func TestEnsureCLIBinCopyFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	exeDir := t.TempDir()
	writeExecutable(t, filepath.Join(exeDir, "astra-cli"), "CLI")
	// 把 <dataDir>/bin 变成一个普通文件，令 MkdirAll(bin) 失败 → 拷贝失败。
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ensureCLIBinFrom(Config{DataDir: dir}, appTestLogger(), "", exeDir)
	if got != "" {
		t.Fatalf("拷贝失败应只记日志并返回空串降级, got %q", got)
	}
}
