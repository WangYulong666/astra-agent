package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// vaultVersion 是落盘格式版本。格式一旦改变必须升版本并在 load 里做迁移，
// 否则老文件会被当成损坏直接拒绝（宁可起不来，也不要把一堆条目解错）。
const vaultVersion = 1

// scopeSep 是 AAD 的分隔符。三个身份字段都不允许包含它，
// 否则 ("a|b","c") 和 ("a","b|c") 会算出同一个 AAD——那就等于没绑定身份。
const scopeSep = "|"

// vaultFile 就是磁盘上的 JSON。
//
// 为什么条文本身不含 token：密文里是 {"token","refresh"} 的 JSON。
// 明文元信息（tenant/user/name/kind）只用于查找和展示，泄漏了也不至于丢凭据；
// 真正的秘密只在 ciphertext 里，且被 AAD 绑死到这三个身份字段上。
type vaultFile struct {
	Version int          `json:"version"`
	Entries []vaultEntry `json:"entries"`
}

type vaultEntry struct {
	Tenant     string     `json:"tenant"`
	User       string     `json:"user"`
	Name       string     `json:"name"`
	Kind       Kind       `json:"kind"`
	Nonce      string     `json:"nonce"`      // base64(std)，每条独立随机
	Ciphertext string     `json:"ciphertext"` // base64(std)，AES-256-GCM 密文+tag
	Expires    *time.Time `json:"expires,omitempty"`
	Scopes     []string   `json:"scopes,omitempty"`
	LinkedAt   time.Time  `json:"linked_at"`
}

// sealedPayload 是密文里的明文结构。
type sealedPayload struct {
	Token   string `json:"token"`
	Refresh string `json:"refresh,omitempty"`
}

// vault 是加解密 + 原子落盘的那一层。
// 所有导出的读写在锁内完成；锁是进程内的（多副本部署应各自独立 vault 或换共享存储）。
type vault struct {
	mu      sync.Mutex
	path    string
	aead    cipher.AEAD
	entries []vaultEntry
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("connector: 主密钥必须 32 字节（AES-256），当前 %d 字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("connector: 初始化 AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("connector: 初始化 GCM: %w", err)
	}
	return aead, nil
}

// openVault 打开（或初始化）凭据库。文件不存在不算错——第一次写入时才创建。
// 已存在但有条目解不开就是错：那说明主密钥换了或文件被改过，静默返回空等于丢凭据。
func openVault(path string, key []byte) (*vault, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("connector: VaultPath 为空")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	v := &vault{path: path, aead: aead}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *vault) load() error {
	data, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("connector: 读凭据库 %s: %w", v.path, err)
	}
	// 老文件权限过松就收紧一次（尤其是有人手抄了这个文件）。
	if fi, statErr := os.Stat(v.path); statErr == nil && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(v.path, 0o600)
	}

	var f vaultFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("connector: 解析凭据库 %s: %w", v.path, err)
	}
	if f.Version != vaultVersion {
		return fmt.Errorf("connector: 凭据库版本 %d 不受支持（本版本只认 %d）", f.Version, vaultVersion)
	}
	// 逐条试解：解不开就整体失败，不把半坏的库交给上层用。
	for _, e := range f.Entries {
		if _, err := v.open(e); err != nil {
			return fmt.Errorf("connector: 凭据库条目 %q 解密失败（主密钥不匹配或文件被篡改）: %w", entryLabel(e), err)
		}
	}
	v.entries = f.Entries
	return nil
}

func entryLabel(e vaultEntry) string {
	return e.Tenant + "/" + e.User + "/" + e.Name
}

// aad 把密文绑到身份上：把 A 的 ciphertext 挪到 B 名下，Open 会直接失败。
func aad(tenantID, userID, name string) []byte {
	return []byte(tenantID + scopeSep + userID + scopeSep + name)
}

func validateIdentity(tenantID, userID, name string) error {
	switch {
	case tenantID == "", userID == "", name == "":
		return errors.New("connector: tenant/user/name 都不能为空")
	case strings.Contains(tenantID, scopeSep), strings.Contains(userID, scopeSep), strings.Contains(name, scopeSep):
		return fmt.Errorf("connector: tenant/user/name 不能包含 %q", scopeSep)
	}
	return nil
}

func (v *vault) seal(tenantID, userID, name string, p sealedPayload) (string, string, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return "", "", fmt.Errorf("connector: 序列化凭据: %w", err)
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("connector: 生成 nonce: %w", err)
	}
	ct := v.aead.Seal(nil, nonce, plain, aad(tenantID, userID, name))
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(ct), nil
}

// open 解密单条。非派生密钥、被改过的密文、AAD 不匹配都会在这里返回错误。
func (v *vault) open(e vaultEntry) (sealedPayload, error) {
	var p sealedPayload
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil {
		return p, fmt.Errorf("connector: nonce 不是合法 base64: %w", err)
	}
	if len(nonce) != v.aead.NonceSize() {
		return p, fmt.Errorf("connector: nonce 长度 %d 非法（期望 %d）", len(nonce), v.aead.NonceSize())
	}
	ct, err := base64.StdEncoding.DecodeString(e.Ciphertext)
	if err != nil {
		return p, fmt.Errorf("connector: 密文不是合法 base64: %w", err)
	}
	plain, err := v.aead.Open(nil, nonce, ct, aad(e.Tenant, e.User, e.Name))
	if err != nil {
		return p, errors.New("connector: 认证失败（密文被篡改，或密文与 tenant/user/name 不匹配）")
	}
	if err := json.Unmarshal(plain, &p); err != nil {
		return p, fmt.Errorf("connector: 明文结构损坏: %w", err)
	}
	return p, nil
}

// set 加密写入并落盘（覆盖同名条目）。
func (v *vault) set(c Credential) error {
	if err := validateIdentity(c.TenantID, c.UserID, c.Name); err != nil {
		return err
	}
	if c.Token == "" {
		return errors.New("connector: token 为空，拒绝写入")
	}
	nonce, ct, err := v.seal(c.TenantID, c.UserID, c.Name, sealedPayload{Token: c.Token, Refresh: c.Refresh})
	if err != nil {
		return err
	}
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	e := vaultEntry{
		Tenant:     c.TenantID,
		User:       c.UserID,
		Name:       c.Name,
		Kind:       c.Kind,
		Nonce:      nonce,
		Ciphertext: ct,
		Scopes:     scopes,
		LinkedAt:   c.LinkedAt,
	}
	if !c.Expires.IsZero() {
		expires := c.Expires
		e.Expires = &expires
	}
	if e.LinkedAt.IsZero() {
		e.LinkedAt = time.Now().UTC()
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	replaced := false
	for i := range v.entries {
		if sameKey(v.entries[i], c.TenantID, c.UserID, c.Name) {
			v.entries[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		v.entries = append(v.entries, e)
	}
	return v.saveLocked()
}

func sameKey(e vaultEntry, tenantID, userID, name string) bool {
	return e.Tenant == tenantID && e.User == userID && e.Name == name
}

// get 解出明文凭据。未连接返回 ErrNotConnected（上层据此提示"去连一下"）。
func (v *vault) get(tenantID, userID, name string) (Credential, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, e := range v.entries {
		if !sameKey(e, tenantID, userID, name) {
			continue
		}
		p, err := v.open(e)
		if err != nil {
			return Credential{}, fmt.Errorf("connector: 凭据 %s 解密失败: %w", entryLabel(e), err)
		}
		cred := Credential{
			TenantID: e.Tenant,
			UserID:   e.User,
			Name:     e.Name,
			Kind:     e.Kind,
			Token:    p.Token,
			Refresh:  p.Refresh,
			Scopes:   e.Scopes,
			LinkedAt: e.LinkedAt,
		}
		if e.Expires != nil {
			cred.Expires = *e.Expires
		}
		return cred, nil
	}
	return Credential{}, ErrNotConnected
}

// list 只返回元信息（Token/Refresh 留空），用于列表接口。
func (v *vault) list(tenantID, userID string) ([]Credential, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]Credential, 0, len(v.entries))
	for _, e := range v.entries {
		if e.Tenant != tenantID || e.User != userID {
			continue
		}
		cred := Credential{
			TenantID: e.Tenant,
			UserID:   e.User,
			Name:     e.Name,
			Kind:     e.Kind,
			Scopes:   e.Scopes,
			LinkedAt: e.LinkedAt,
		}
		if e.Expires != nil {
			cred.Expires = *e.Expires
		}
		out = append(out, cred)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (v *vault) delete(tenantID, userID, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, e := range v.entries {
		if !sameKey(e, tenantID, userID, name) {
			continue
		}
		v.entries = append(v.entries[:i], v.entries[i+1:]...)
		return v.saveLocked()
	}
	return ErrNotFound
}

// saveLocked 写临时文件 + fsync + rename：进程被杀也不会留下半个 JSON。
// 临时文件用 os.CreateTemp（默认 0600），rename 在同目录内是原子的。
func (v *vault) saveLocked() error {
	entries := v.entries
	if entries == nil {
		entries = []vaultEntry{}
	}
	data, err := json.MarshalIndent(vaultFile{Version: vaultVersion, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("connector: 序列化凭据库: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("connector: 创建凭据库目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".vault-*.tmp")
	if err != nil {
		return fmt.Errorf("connector: 创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("connector: 设置临时文件权限: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("connector: 写临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("connector: fsync 凭据库: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("connector: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpName, v.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("connector: 落盘凭据库 %s: %w", v.path, err)
	}
	return nil
}
