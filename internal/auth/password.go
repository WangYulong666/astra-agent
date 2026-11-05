package auth

// 密码哈希与凭据生成。这两个是 T3（管理 API）会直接调用的，
// 所以先落地，避免下游因跨包缺函数而无法编译。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 12 ≈ 每次哈希 200~400ms。
//
// 为什么不用默认的 10：10 在现代 GPU 上已经偏快。
// 为什么不上 14：登录接口会被拖到 1s+，用户能感知到卡。
// 12 是「攻击成本」和「登录手感」的折中。
const bcryptCost = 12

// MinPasswordLen 最短密码长度。不强制复杂度规则——那只会逼出 Passw0rd!，
// 长度才是真正有效的熵来源。
const MinPasswordLen = 8

// HashPassword 生成 bcrypt 哈希。
func HashPassword(pw string) (string, error) {
	if len([]rune(pw)) < MinPasswordLen {
		return "", fmt.Errorf("密码至少 %d 位", MinPasswordLen)
	}
	// bcrypt 只取前 72 字节，超出部分会被静默截断。截断比报错更危险（两个不同密码等价），
	// 所以这里显式拒绝。
	if len(pw) > 72 {
		return "", errors.New("密码不能超过 72 字节")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验密码。失败统一返回 ErrInvalidCredential，
// 不区分「用户不存在」和「密码错误」——区分了就等于送给攻击者一个账号枚举接口。
func CheckPassword(hash, pw string) error {
	if hash == "" {
		return ErrInvalidCredential
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)); err != nil {
		return ErrInvalidCredential
	}
	return nil
}

// GenerateAPIKey 生成长期凭据。
//
// 返回值：
//   - plain  完整明文，只在创建响应里出现一次，之后无从取回
//   - prefix 展示用前缀（含 astra_sk_），列表里用来辨认是哪把钥匙
//   - hash   sha256(plain) 的 hex，入库
//
// 为什么用 sha256 而不是 bcrypt：API Key 是 32 字节高熵随机串，
// 不存在被字典攻击的前提，而每次请求都要校验——bcrypt 会让请求慢 300ms。
func GenerateAPIKey() (plain, prefix, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plain = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	if len(plain) < APIKeyPrefixLen {
		return "", "", "", errors.New("凭据长度异常")
	}
	prefix = plain[:APIKeyPrefixLen]
	return plain, prefix, HashAPIKey(plain), nil
}

// HashAPIKey 计算凭据摘要。校验时对明文重算再比对。
func HashAPIKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// GeneratePassword 生成一个临时密码，用于 bootstrap 与重置密码。
//
// 用 base64url 而不是可读单词组合：临时密码只在终端打印一次，
// 不需要好记，需要的是熵足够且不含 shell 元字符（避免复制粘贴时被转义）。
func GeneratePassword() (string, error) {
	buf := make([]byte, 12) // 96 bit → 16 字符
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RandomID 生成 26 字符的标识，与 store 的 ID 风格保持一致。
func RandomID() (string, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	buf := make([]byte, 26)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}
