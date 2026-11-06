package connector

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("生成测试主密钥失败: %v", err)
	}
	return key
}

func newTestVault(t *testing.T, path string, key []byte) *vault {
	t.Helper()
	v, err := openVault(path, key)
	if err != nil {
		t.Fatalf("openVault: %v", err)
	}
	return v
}

func sampleCred(tenant, user, name string) Credential {
	return Credential{
		TenantID: tenant,
		UserID:   user,
		Name:     name,
		Kind:     KindOAuth2,
		Token:    "gho_secret_access_token",
		Refresh:  "ghr_secret_refresh_token",
		Expires:  time.Now().UTC().Add(time.Hour).Truncate(time.Second),
		Scopes:   []string{"repo", "read:user"},
		LinkedAt: time.Now().UTC().Truncate(time.Second),
	}
}

func TestVaultRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	key := testKey(t)

	v := newTestVault(t, path, key)
	in := sampleCred("t1", "u1", "github")
	if err := v.set(in); err != nil {
		t.Fatalf("set: %v", err)
	}

	// 重新打开（模拟进程重启）必须还能解出同样的明文。
	v2 := newTestVault(t, path, key)
	out, err := v2.get("t1", "u1", "github")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.Token != in.Token || out.Refresh != in.Refresh {
		t.Fatalf("明文不一致: token=%q refresh=%q", out.Token, out.Refresh)
	}
	if !out.Expires.Equal(in.Expires) {
		t.Fatalf("expires 不一致: got %v want %v", out.Expires, in.Expires)
	}
	if len(out.Scopes) != 2 || out.Scopes[0] != "repo" {
		t.Fatalf("scopes 不一致: %v", out.Scopes)
	}
	if out.Kind != KindOAuth2 {
		t.Fatalf("kind 不一致: %v", out.Kind)
	}
}

func TestVaultCiphertextIsNotPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	v := newTestVault(t, path, testKey(t))
	cred := sampleCred("t1", "u1", "github")
	if err := v.set(cred); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if s := string(raw); strings.Contains(s, cred.Token) || strings.Contains(s, cred.Refresh) {
		t.Fatalf("落盘文件里出现了明文 token：\n%s", s)
	}
}

func TestVaultTamperedCiphertextFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	key := testKey(t)
	v := newTestVault(t, path, key)
	if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
		t.Fatalf("set: %v", err)
	}

	f := readVaultFile(t, path)
	ct, err := base64.StdEncoding.DecodeString(f.Entries[0].Ciphertext)
	if err != nil {
		t.Fatalf("解 base64: %v", err)
	}
	ct[len(ct)-1] ^= 0xFF // 翻转 tag 的最后一个字节
	f.Entries[0].Ciphertext = base64.StdEncoding.EncodeToString(ct)
	writeVaultFile(t, path, f)

	if _, err := openVault(path, key); err == nil {
		t.Fatal("篡改密文后 openVault 应当报错，实际返回 nil")
	}
}

func TestVaultAADMismatchRejected(t *testing.T) {
	cases := []struct {
		name  string
		mutau func(*vaultEntry)
	}{
		{"换名字（A 的密文挂到 B 名下）", func(e *vaultEntry) { e.Name = "google" }},
		{"换租户", func(e *vaultEntry) { e.Tenant = "t2" }},
		{"换用户", func(e *vaultEntry) { e.User = "u2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vault.json")
			key := testKey(t)
			v := newTestVault(t, path, key)
			if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
				t.Fatalf("set: %v", err)
			}

			f := readVaultFile(t, path)
			tc.mutau(&f.Entries[0])
			writeVaultFile(t, path, f)

			// 加载必须失败：AAD 把密文绑死到 tenant|user|name，挪位置就解不开。
			if _, err := openVault(path, key); err == nil {
				t.Fatal("AAD 错配后 openVault 应当报错，实际返回 nil")
			}
			// 就算库已经在内存里，按新身份也取不到东西。
			if _, err := v.get(f.Entries[0].Tenant, f.Entries[0].User, f.Entries[0].Name); err == nil {
				t.Fatal("AAD 错配后 get 应当报错")
			}
		})
	}
}

func TestVaultWrongMasterKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	v := newTestVault(t, path, testKey(t))
	if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := openVault(path, testKey(t)); err == nil {
		t.Fatal("换了主密钥后 openVault 应当报错（否则会静默丢凭据）")
	}
}

func TestVaultExplicitNonceLengthRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	key := testKey(t)
	v := newTestVault(t, path, key)
	if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
		t.Fatalf("set: %v", err)
	}
	f := readVaultFile(t, path)
	f.Entries[0].Nonce = base64.StdEncoding.EncodeToString([]byte("short"))
	writeVaultFile(t, path, f)
	if _, err := openVault(path, key); err == nil {
		t.Fatal("nonce 长度不对应当报错")
	}
}

func TestVaultFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "vault.json")
	v := newTestVault(t, path, testKey(t))
	if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
		t.Fatalf("set: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("凭据库权限应为 0600，实际 %04o", perm)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err == nil {
		if perm := dir.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("凭据库目录不应对同组/其他用户开放，实际 %04o", perm)
		}
	}
}

func TestVaultConcurrentWritesKeepFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	key := testKey(t)
	v := newTestVault(t, path, key)

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cred := sampleCred("t1", "u1", fmt.Sprintf("conn-%02d", i))
			cred.Token = fmt.Sprintf("token-%02d", i)
			if err := v.set(cred); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发写入失败: %v", err)
	}

	// 文件必须是完整可解析的 JSON，且 32 条一条不少。
	f := readVaultFile(t, path)
	if len(f.Entries) != n {
		t.Fatalf("条目数 %d，期望 %d（有写入丢失，说明原子写没做好）", len(f.Entries), n)
	}
	v2 := newTestVault(t, path, key)
	creds, err := v2.list("t1", "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(creds) != n {
		t.Fatalf("重开后条目数 %d，期望 %d", len(creds), n)
	}
	for i, c := range creds {
		want := fmt.Sprintf("conn-%02d", i)
		if c.Name != want {
			t.Fatalf("第 %d 条名字 %q，期望 %q（list 应按名字排序）", i, c.Name, want)
		}
		if c.Token != "" || c.Refresh != "" {
			t.Fatalf("list 不应返回 token: %+v", c)
		}
	}
}

func TestVaultDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	v := newTestVault(t, path, testKey(t))
	if err := v.set(sampleCred("t1", "u1", "github")); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := v.delete("t1", "u1", "github"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := v.get("t1", "u1", "github"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("删除后应返回 ErrNotConnected，实际 %v", err)
	}
	if err := v.delete("t1", "u1", "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际 %v", err)
	}
}

func TestVaultRejectsBadIdentityAndEmptyToken(t *testing.T) {
	v := newTestVault(t, filepath.Join(t.TempDir(), "vault.json"), testKey(t))
	bad := sampleCred("t|1", "u1", "github")
	if err := v.set(bad); err == nil {
		t.Fatal("身份字段含分隔符时应当拒绝（否则 AAD 会串位）")
	}
	empty := sampleCred("t1", "u1", "github")
	empty.Token = ""
	if err := v.set(empty); err == nil {
		t.Fatal("空 token 应当拒绝")
	}
}

// ---- 测试辅助 ----

func readVaultFile(t *testing.T, path string) vaultFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读凭据库: %v", err)
	}
	var f vaultFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析凭据库: %v\n%s", err, raw)
	}
	return f
}

func writeVaultFile(t *testing.T, path string, f vaultFile) {
	t.Helper()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写凭据库: %v", err)
	}
}
