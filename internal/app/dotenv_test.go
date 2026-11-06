package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	// 用带随机后缀的键名，避免撞上 CI 环境里已存在的变量
	keys := []string{
		"ASTRA_TEST_PLAIN", "ASTRA_TEST_TRAILING_COMMENT", "ASTRA_TEST_EMPTY_COMMENT",
		"ASTRA_TEST_QUOTED_HASH", "ASTRA_TEST_INNER_HASH", "ASTRA_TEST_EXPORT",
		"ASTRA_TEST_ALREADY_SET",
	}
	for _, k := range keys {
		k := k
		t.Cleanup(func() { _ = os.Unsetenv(k) })
		_ = os.Unsetenv(k)
	}

	content := `# 整行注释
ASTRA_TEST_PLAIN=hello
ASTRA_TEST_TRAILING_COMMENT=value   # 这是注释
ASTRA_TEST_EMPTY_COMMENT=    # 留空=关闭能力
ASTRA_TEST_QUOTED_HASH="a # b"
ASTRA_TEST_INNER_HASH=abc#def
export ASTRA_TEST_EXPORT=exported
ASTRA_TEST_ALREADY_SET=from_file
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写 .env: %v", err)
	}

	// shell 里已 export 的优先，不被文件覆盖
	if err := os.Setenv("ASTRA_TEST_ALREADY_SET", "from_shell"); err != nil {
		t.Fatal(err)
	}

	if n := LoadDotEnv(path, nil); n != len(keys)-1 {
		t.Fatalf("应加载 %d 个（已设置的跳过），实际 %d", len(keys)-1, n)
	}

	want := map[string]string{
		"ASTRA_TEST_PLAIN":            "hello",
		"ASTRA_TEST_TRAILING_COMMENT": "value",
		"ASTRA_TEST_EMPTY_COMMENT":    "",
		"ASTRA_TEST_QUOTED_HASH":      "a # b",
		"ASTRA_TEST_INNER_HASH":       "abc#def",
		"ASTRA_TEST_EXPORT":           "exported",
		"ASTRA_TEST_ALREADY_SET":      "from_shell",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q，期望 %q", k, got, v)
		}
	}

	// 文件不存在是正常情况，不该报错也不该加载
	if n := LoadDotEnv(filepath.Join(dir, "missing"), nil); n != 0 {
		t.Errorf("文件不存在应返回 0，实际 %d", n)
	}
}
