package connector

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	m := newTestManager(t, nil)
	now := time.Now()

	state, err := m.signState("tenant-1", "user-1", "github", now)
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	// state 只有两段：payload 与签名，中间不带任何服务端内存状态。
	if strings.Count(state, ".") != 1 {
		t.Fatalf("state 结构应为 base64url(payload).base64url(sig)，实际 %q", state)
	}
	tenant, user, name, err := m.ParseState(state)
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if tenant != "tenant-1" || user != "user-1" || name != "github" {
		t.Fatalf("解析结果不对: %s/%s/%s", tenant, user, name)
	}
}

func TestStateExpired(t *testing.T) {
	m := newTestManager(t, nil)
	state, err := m.signState("t1", "u1", "github", time.Now().Add(-11*time.Minute))
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	if _, _, _, err := m.ParseState(state); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("过期 state 应被拒绝，实际 %v", err)
	}
}

func TestStateJustInsideTTL(t *testing.T) {
	m := newTestManager(t, nil)
	state, err := m.signState("t1", "u1", "github", time.Now().Add(-9*time.Minute))
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	if _, _, _, err := m.ParseState(state); err != nil {
		t.Fatalf("9 分钟前签的 state 仍在 10 分钟有效期内，应通过: %v", err)
	}
}

func TestStateFuture(t *testing.T) {
	m := newTestManager(t, nil)
	state, err := m.signState("t1", "u1", "github", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	if _, _, _, err := m.ParseState(state); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("时间戳在未来的 state 应被拒绝，实际 %v", err)
	}
}

func TestStateForgedSignature(t *testing.T) {
	m := newTestManager(t, nil)
	state, err := m.signState("t1", "u1", "github", time.Now())
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	payload, sig, _ := strings.Cut(state, ".")
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("解签名: %v", err)
	}
	rawSig[0] ^= 0x01
	forged := payload + "." + base64.RawURLEncoding.EncodeToString(rawSig)
	if _, _, _, err := m.ParseState(forged); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("伪造签名应被拒绝，实际 %v", err)
	}
}

func TestStateTamperedPayload(t *testing.T) {
	m := newTestManager(t, nil)
	state, err := m.signState("t1", "u1", "github", time.Now())
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	_, sig, _ := strings.Cut(state, ".")
	// 保留原签名、把 payload 换成别人的身份：签名校验必须挡住。
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	tampered := base64.RawURLEncoding.EncodeToString([]byte("t2|u2|github|"+stamp)) + "." + sig
	if _, _, _, err := m.ParseState(tampered); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("篡改 payload 应被拒绝，实际 %v", err)
	}
}

func TestStateSignedWithAnotherKeyRejected(t *testing.T) {
	a := newTestManager(t, nil)
	b := newTestManager(t, nil) // 不同的 VaultKey → 不同的派生签名密钥
	state, err := a.signState("t1", "u1", "github", time.Now())
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	if _, _, _, err := b.ParseState(state); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("跨实例（不同主密钥）签的 state 应被拒绝，实际 %v", err)
	}
}

func TestStateMalformed(t *testing.T) {
	m := newTestManager(t, nil)
	valid, err := m.signState("t1", "u1", "github", time.Now())
	if err != nil {
		t.Fatalf("signState: %v", err)
	}
	payload, sig, _ := strings.Cut(valid, ".")

	cases := map[string]string{
		"空串":                "",
		"没有点":               "abcdef",
		"三段":                "a.b.c",
		"payload 多了字段":      base64.RawURLEncoding.EncodeToString([]byte("t1|u1|github|1|extra")) + "." + sig,
		"payload 少了字段":      base64.RawURLEncoding.EncodeToString([]byte("t1|u1|1")) + "." + sig,
		"payload 不是 base64": "!!!." + sig,
		"签名字段不是 base64":     payload + ".???",
		"payload 时间戳非法":     base64.RawURLEncoding.EncodeToString([]byte("t1|u1|github|notanumber")) + "." + sig,
	}
	for name, state := range cases {
		if _, _, _, err := m.ParseState(state); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%s: 应被拒绝，实际 %v", name, err)
		}
	}
}

func TestStateDisabled(t *testing.T) {
	m := newTestManager(t, func(c *Config) { c.Enabled = false })
	if _, _, _, err := m.ParseState("whatever"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("未启用时应返回 ErrDisabled，实际 %v", err)
	}
}
