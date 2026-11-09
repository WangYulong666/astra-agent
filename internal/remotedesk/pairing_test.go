package remotedesk

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestStartPairingShapeAndPayload(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()

	before := time.Now()
	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1", Device: "MacBook Pro"})
	if err != nil {
		t.Fatalf("StartPairing 失败: %v", err)
	}

	if !regexp.MustCompile(`^\d{6}$`).MatchString(p.Code) {
		t.Fatalf("配对码应是 6 位数字，实际 %q", p.Code)
	}
	ttl := p.ExpiresAt.Sub(before)
	if ttl < 100*time.Second || ttl > 130*time.Second {
		t.Fatalf("默认 TTL 应约 120 秒，实际 %v", ttl)
	}
	if !strings.HasPrefix(p.Payload, "astra://remote/pair?") {
		t.Fatalf("二维码内容应是深链，实际 %q", p.Payload)
	}
	if !strings.Contains(p.Payload, "code="+p.Code) {
		t.Fatalf("二维码应带 code，实际 %q", p.Payload)
	}
	// 二维码会被拍照转发，绝不能塞长期凭据。
	if strings.Contains(strings.ToLower(p.Payload), "token") || strings.Contains(strings.ToLower(p.Payload), "secret") {
		t.Fatalf("二维码里不许出现 token: %q", p.Payload)
	}
	if p.DeepLink != p.Payload {
		t.Fatalf("DeepLink 应与 Payload 同值（前端只画一份），实际 %q vs %q", p.DeepLink, p.Payload)
	}
}

func TestClaimPairingCreatesDevice(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()

	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1", Device: "MacBook Pro"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := env.m.ClaimPairing(ctx, ClaimRequest{
		Code: p.Code, TenantID: "t1", UserID: "u1", Device: "MacBook Pro", Platform: "darwin",
	})
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	if !regexp.MustCompile(`^dev_[0-9a-f]{12}$`).MatchString(d.ID) {
		t.Fatalf("设备 ID 应是 dev_ + 12 位 hex，实际 %q", d.ID)
	}
	if d.CreatedAt.IsZero() || d.LastSeenAt.IsZero() {
		t.Fatal("设备应记录创建/最后在线时间")
	}
	if d.Online {
		t.Fatal("刚配对的设备还没有会话，不该显示在线")
	}

	list, err := env.m.ListDevices(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("设备列表应有刚配对的设备，实际 %+v", list)
	}

	ev, ok := env.aud.find("remote.pair")
	if !ok {
		t.Fatal("缺少 remote.pair 审计")
	}
	if ev.Target != d.ID {
		t.Fatalf("审计 target 应是设备 ID，实际 %q", ev.Target)
	}
	if _, leaked := ev.Detail["code"]; leaked {
		t.Fatal("审计里不许记配对码")
	}
	for _, v := range ev.Detail {
		if s, _ := v.(string); s == p.Code {
			t.Fatal("审计 detail 里出现了配对码")
		}
	}
}

func TestPairingCodeIsSingleUse(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("同一个码不能用两次，应 ErrNotFound，实际 %v", err)
	}
}

func TestPairingCodeExpires(t *testing.T) {
	env := r1Open(t, func(c *Config) { c.Policy.PairingTTL = 30 * time.Millisecond })
	ctx := context.Background()
	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期码应 ErrNotFound，实际 %v", err)
	}
	// 过期后再用同样的码也不行（已被清掉）。
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期码第二次也应 ErrNotFound，实际 %v", err)
	}
}

func TestPairingBruteForceBurnsCode(t *testing.T) {
	env := r1Open(t, func(c *Config) { c.Policy.PairingMaxAttempts = 5 })
	ctx := context.Background()
	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if wrong == p.Code {
		wrong = "111111"
	}
	for i := 1; i <= 5; i++ {
		if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: wrong, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrDenied) {
			t.Fatalf("第 %d 次错码应 ErrDenied，实际 %v", i, err)
		}
	}
	// 达上限后码作废：连正确的码也不能再用（否则爆破只是「多试几次」）。
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("错 5 次后码应作废，实际 %v", err)
	}
	if _, err := env.m.ListDevices(ctx, "t1", "u1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := env.m.ListDevices(ctx, "t1", "u1"); len(list) != 0 {
		t.Fatalf("爆破失败不该产生设备，实际 %+v", list)
	}
}

func TestPairingRegenerateInvalidatesOld(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	first, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Code == second.Code {
		t.Skip("两次随机到同一个码，重跑即可")
	}
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: first.Code, TenantID: "t1", UserID: "u1"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("旧码应被新码顶掉，实际 %v", err)
	}
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: second.Code, TenantID: "t1", UserID: "u1"}); err != nil {
		t.Fatalf("新码应可用，实际 %v", err)
	}
}

func TestPairingCrossTenantIsNotFound(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	p, err := env.m.StartPairing(ctx, PairingRequest{TenantID: "t1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// 别的租户拿着同一个码来认领：一律「不存在」，不泄露「这个码是别人的」。
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t2", UserID: "u1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户认领应 ErrNotFound，实际 %v", err)
	}
	// 同租户不同用户也一样。
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u2"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户认领应 ErrNotFound，实际 %v", err)
	}
	// 原主不受影响。
	if _, err := env.m.ClaimPairing(ctx, ClaimRequest{Code: p.Code, TenantID: "t1", UserID: "u1"}); err != nil {
		t.Fatalf("别人的失败尝试不该消耗原主的码: %v", err)
	}
}

func TestListDevicesIsolatesTenantsAndTracksOnline(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d1 := r1Pair(t, env.m, "t1", "u1", "Mac 1")
	d2 := r1Pair(t, env.m, "t1", "u1", "Mac 2")
	_ = r1Pair(t, env.m, "t2", "u9", "别人的 Mac")

	list, err := env.m.ListDevices(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("只该看到自己的 2 台设备，实际 %d", len(list))
	}
	if list[0].Name != "Mac 2" {
		t.Fatalf("应按创建时间倒序，实际首条 %q", list[0].Name)
	}

	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d1.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	list, _ = env.m.ListDevices(ctx, "t1", "u1")
	online := map[string]bool{}
	for _, d := range list {
		online[d.ID] = d.Online
	}
	if !online[d1.ID] {
		t.Fatal("有 active 会话的设备应显示在线")
	}
	if online[d2.ID] {
		t.Fatal("没有会话的设备不该显示在线")
	}

	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); err != nil {
		t.Fatal(err)
	}
	list, _ = env.m.ListDevices(ctx, "t1", "u1")
	for _, d := range list {
		if d.ID == d1.ID && d.Online {
			t.Fatal("会话结束后不该显示在线")
		}
	}

	if other, _ := env.m.ListDevices(ctx, "t2", "u9"); len(other) != 1 {
		t.Fatalf("另一个租户应只看到自己的设备，实际 %d", len(other))
	}
	if none, _ := env.m.ListDevices(ctx, "t1", "u9"); len(none) != 0 {
		t.Fatalf("租户内不同用户不该看到别人的设备，实际 %d", len(none))
	}
}

func TestRevokeDeviceCascadesSessions(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")

	view, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if err != nil {
		t.Fatal(err)
	}

	// 跨租户吊销：不存在。
	if err := env.m.RevokeDevice(ctx, "t2", "u1", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户吊销应 ErrNotFound，实际 %v", err)
	}
	if err := env.m.RevokeDevice(ctx, "t1", "u1", d.ID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}

	for _, id := range []string{view.ID, ctrl.ID} {
		s, err := env.m.GetSession(ctx, "t1", id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Status != StatusEnded || s.EndReason != ReasonRevoked {
			t.Fatalf("吊销应级联断开会话，实际 %s/%s", s.Status, s.EndReason)
		}
	}

	list, err := env.m.ListDevices(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("已吊销设备不该出现在列表，实际 %+v", list)
	}

	// 幂等。
	if err := env.m.RevokeDevice(ctx, "t1", "u1", d.ID); err != nil {
		t.Fatalf("重复吊销应幂等，实际 %v", err)
	}
	if env.aud.count("remote.revoke") != 1 {
		t.Fatalf("只该记一次 remote.revoke，实际 %d", env.aud.count("remote.revoke"))
	}
	if err := env.m.RevokeDevice(ctx, "t1", "u1", "dev_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("吊销不存在的设备应 ErrNotFound，实际 %v", err)
	}
}
