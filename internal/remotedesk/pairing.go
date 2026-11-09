package remotedesk

// 配对与设备。
//
// 配对码只证明「人此刻就在电脑跟前」：它一次性、120 秒、失败 5 次作废，
// 而且只在内存里——重启就全失效（旧码本来就该失效）。
//
// 归属口径（与平台其它能力一致）：设备的 TenantID/UserID 来自配对发起方的
// principal；列表、吊销、建会话都按「tenant + user 全等」过滤，
// 不匹配一律 ErrNotFound，**不区分「不存在」与「不是你的」**，避免探测别人的设备。

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"time"
)

type pairState struct {
	code      string
	expiresAt time.Time
	device    string
	attempts  int
}

func pairKey(tenantID, userID string) string { return tenantID + "\x00" + userID }

func (p Policy) pairingTTL() time.Duration { return p.WithDefaults().PairingTTL }

// newPairingCode 生成 6 位数字码。
// 用 crypto/rand 而不是 math/rand：配对码是「谁先猜中谁就能绑设备」的凭据，
// math/rand 的种子可预测，等于把绑定权直接送人。
func newPairingCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// subtleCompare 定长比较。长度不同直接不匹配（ConstantTimeCompare 的行为）。
func subtleCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}

// urlQueryEscape 只用于拼深链参数：设备名/服务地址可能带 & # 空格，
// 不转义会把查询串拆坏。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

func (m *manager) StartPairing(ctx context.Context, req PairingRequest) (Pairing, error) {
	if err := m.ensureOpen(); err != nil {
		return Pairing{}, err
	}
	if err := ctx.Err(); err != nil {
		return Pairing{}, err
	}
	code, err := newPairingCode()
	if err != nil {
		return Pairing{}, fmt.Errorf("%w: 生成配对码失败: %v", ErrUnavailable, err)
	}
	now := time.Now()
	p := Pairing{
		Code:      code,
		ExpiresAt: now.Add(m.pol.pairingTTL()),
		// 服务地址不在 Config 里（部署方各自决定反向代理域名），留空由前端补。
		Payload: pairingPayload(code, "", req.Device),
	}
	// DeepLink 与二维码同值：原生 App 直接跳，网页端画 QR。
	p.DeepLink = p.Payload

	m.pairMu.Lock()
	// 同一 tenant/user 重新生成时旧的立刻作废，避免同时有两个有效码。
	m.pairings[pairKey(req.TenantID, req.UserID)] = &pairState{
		code:      code,
		expiresAt: p.ExpiresAt,
		device:    req.Device,
	}
	m.pairMu.Unlock()

	return p, nil
}

// pairingPayload 拼二维码内容。
// **不放 token**：二维码会被拍照、转发、截图贴群里，任何长期凭据塞进去都等于泄露。
// 只放一次性 code + 服务地址 + 设备名提示。
func pairingPayload(code, server, name string) string {
	parts := []string{"code=" + urlQueryEscape(code)}
	if server != "" {
		parts = append(parts, "server="+urlQueryEscape(server))
	}
	if name != "" {
		parts = append(parts, "name="+urlQueryEscape(name))
	}
	return "astra://remote/pair?" + strings.Join(parts, "&")
}

func (m *manager) ClaimPairing(ctx context.Context, req ClaimRequest) (Device, error) {
	if err := m.ensureOpen(); err != nil {
		return Device{}, err
	}
	if err := ctx.Err(); err != nil {
		return Device{}, err
	}
	key := pairKey(req.TenantID, req.UserID)
	code := strings.TrimSpace(req.Code)

	m.pairMu.Lock()
	st, ok := m.pairings[key]
	if !ok {
		m.pairMu.Unlock()
		// 跨租户/跨用户也走这里：不区分「没有」与「不是你的」，避免探测别人的配对码。
		return Device{}, ErrNotFound
	}
	now := time.Now()
	if !now.Before(st.expiresAt) {
		delete(m.pairings, key)
		m.pairMu.Unlock()
		return Device{}, ErrNotFound
	}
	// 定长比较，别用 == ：用户输入路径上的比较一律按敏感数据处理，
	// 虽然码在内存里不经过网络时序，习惯比场景更重要。
	if subtleCompare(st.code, code) != 1 {
		st.attempts++
		attempts := st.attempts
		limit := m.pol.PairingMaxAttempts
		burnt := attempts >= limit
		if burnt {
			delete(m.pairings, key)
		}
		m.pairMu.Unlock()
		// 不记 code 本身，只记次数：6 位码就 100 万种，日志里记了等于帮人爆破。
		m.logf("配对被拒（tenant=%s user=%s 第 %d/%d 次），达上限则作废", req.TenantID, req.UserID, attempts, limit)
		return Device{}, ErrDenied
	}
	deviceName := strings.TrimSpace(req.Device)
	if deviceName == "" {
		deviceName = strings.TrimSpace(st.device)
	}
	if deviceName == "" {
		deviceName = "未命名设备"
	}
	delete(m.pairings, key)
	m.pairMu.Unlock()

	dev := Device{
		ID:         newDeviceID(),
		TenantID:   req.TenantID,
		UserID:     req.UserID,
		Name:       deviceName,
		Platform:   req.Platform,
		CreatedAt:  now,
		LastSeenAt: now,
	}

	m.mu.Lock()
	m.devices[dev.ID] = &dev
	m.pruneDevicesLocked()
	err := m.saveDevicesLocked()
	m.mu.Unlock()
	if err != nil {
		return Device{}, err
	}

	// 审计只记设备与机型，不记 code（理由同上）。
	m.emitAudit("remote.pair", dev.ID, map[string]any{
		"device":   dev.Name,
		"platform": dev.Platform,
	})
	return cloneDevice(&dev), nil
}

func (m *manager) ListDevices(ctx context.Context, tenantID, userID string) ([]Device, error) {
	if err := m.ensureOpen(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	out := make([]Device, 0, len(m.devices))
	for _, d := range m.devices {
		if d.RevokedAt != nil {
			continue
		}
		if d.TenantID != tenantID || d.UserID != userID {
			continue
		}
		cd := cloneDevice(d)
		cd.Online = m.onlineLocked(d.ID)
		out = append(out, cd)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// onlineLocked 由「有没有 active 会话」推导，不落盘（重启后自然为 false）。
func (m *manager) onlineLocked(deviceID string) bool {
	for _, s := range m.sessions {
		if s.DeviceID == deviceID && s.Status == StatusActive {
			return true
		}
	}
	return false
}

func (m *manager) RevokeDevice(ctx context.Context, tenantID, userID, deviceID string) error {
	if err := m.ensureOpen(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var cascaded []string
	m.mu.Lock()
	dev, ok := m.devices[deviceID]
	if !ok || dev.TenantID != tenantID || dev.UserID != userID {
		m.mu.Unlock()
		return ErrNotFound
	}
	if dev.RevokedAt != nil {
		m.mu.Unlock()
		return nil // 幂等：重复吊销不算错
	}
	now := time.Now()
	dev.RevokedAt = &now
	dev.Online = false
	// 吊销必须立刻断掉现有会话：只标记设备不踢会话，等于「拉黑了但门还开着」。
	for _, s := range m.sessions {
		if s.DeviceID == deviceID && !isFinished(s.Status) {
			s.Status = StatusEnded
			s.EndedAt = &now
			s.EndReason = ReasonRevoked
			cascaded = append(cascaded, s.ID)
		}
	}
	err := m.saveDevicesLocked()
	if err == nil {
		err = m.saveSessionsLocked()
	}
	name := dev.Name
	m.mu.Unlock()
	if err != nil {
		return err
	}

	m.closeRuntimes(cascaded...)
	m.emitAudit("remote.revoke", deviceID, map[string]any{
		"device":   name,
		"sessions": len(cascaded),
	})
	return nil
}
