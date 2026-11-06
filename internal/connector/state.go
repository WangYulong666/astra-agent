package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// state 必须自包含（签名，不靠服务端内存）：OAuth 回调是浏览器直达的，
// 多副本部署时可能打到另一个副本上，内存 map 会直接失效。
//
//	state = base64url(tenant|user|name|unixTs) + "." + base64url(HMAC-SHA256(payload, key))
//
// key 从 VaultKey 派生，不新增配置项：state 的伪造门槛就等于主密钥。

const (
	stateTTL    = 10 * time.Minute
	stateLeeway = 1 * time.Minute // 容忍前端/网关时钟略快
	stateParts  = 4
)

// deriveStateKey 用 HMAC(vaultKey, "state") 做域分离：
// 即使将来把主密钥直接用于别处，也不会和 state 签名共用同一把密钥。
func deriveStateKey(vaultKey []byte) []byte {
	mac := hmac.New(sha256.New, vaultKey)
	mac.Write([]byte("state"))
	return mac.Sum(nil)
}

// signState 生成自包含 state。tenant/user/name 不能含 "|"，否则解析时字段会串位。
func (m *manager) signState(tenantID, userID, name string, now time.Time) (string, error) {
	if err := validateIdentity(tenantID, userID, name); err != nil {
		return "", err
	}
	payload := strings.Join([]string{tenantID, userID, name, strconv.FormatInt(now.Unix(), 10)}, scopeSep)
	return m.signPayload(payload), nil
}

func (m *manager) signPayload(payload string) string {
	mac := hmac.New(sha256.New, m.stateKey)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// ParseState 校验签名与时效，返回授权上下文。
// 所有失败都只回一个笼统的 ErrInvalidState（不区分「签名不对」和「过期」），
// 免得把校验细节当预言机送给攻击者。
func (m *manager) ParseState(state string) (tenantID, userID, name string, err error) {
	if !m.Enabled() {
		return "", "", "", ErrDisabled
	}
	if state == "" {
		return "", "", "", ErrInvalidState
	}
	encoded, encodedSig, ok := strings.Cut(state, ".")
	if !ok {
		return "", "", "", ErrInvalidState
	}
	payload, decErr := base64.RawURLEncoding.DecodeString(encoded)
	if decErr != nil {
		return "", "", "", ErrInvalidState
	}
	sig, decErr := base64.RawURLEncoding.DecodeString(encodedSig)
	if decErr != nil {
		return "", "", "", ErrInvalidState
	}

	// 先验签再解析：未通过签名的 payload 不值得解析。
	mac := hmac.New(sha256.New, m.stateKey)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", "", "", ErrInvalidState
	}

	parts := strings.Split(string(payload), scopeSep)
	if len(parts) != stateParts {
		return "", "", "", ErrInvalidState
	}
	tenantID, userID, name = parts[0], parts[1], parts[2]
	if tenantID == "" || userID == "" || name == "" {
		return "", "", "", ErrInvalidState
	}
	ts, convErr := strconv.ParseInt(parts[3], 10, 64)
	if convErr != nil {
		return "", "", "", ErrInvalidState
	}
	issued := time.Unix(ts, 0)
	now := time.Now()
	if now.Sub(issued) > stateTTL {
		return "", "", "", fmt.Errorf("%w: 已过期", ErrInvalidState)
	}
	if issued.Sub(now) > stateLeeway {
		return "", "", "", fmt.Errorf("%w: 时间戳在未来", ErrInvalidState)
	}
	return tenantID, userID, name, nil
}

// ErrInvalidState 表示 OAuth 回调带的 state 不可信（签名错/过期/格式坏）。
var ErrInvalidState = errors.New("connector: state 无效或已过期")
