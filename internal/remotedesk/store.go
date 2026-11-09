package remotedesk

// 设备 / 会话的落盘层。
//
// 为什么是 JSON 文件而不是数据库：量级极小（设备 ≤200、会话 ≤500，都是「每台机器
// 自己的配对关系」），查询模式只有「按租户+用户列」和「按 ID 取」，而且这些数据必须
// 跟着 DataDir 一起被备份/迁移。files store 与 sql store 两种部署下都要能工作，
// 所以不引入表。
//
// 并发：进程内 sync.RWMutex（由 manager 持有），写全部走
// 「临时文件 + fsync + rename」，任何时刻磁盘上都是完整的新版或旧版。
// 读坏了**必须报错**——设备/会话丢了要让人立刻知道，而不是静默当首启，
// 否则用户看到的就是「配对关系莫名其妙全没了」。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// maxDevices / maxSessions 是落盘条数上限，超出按 FIFO 淘汰。
	maxDevices  = 200
	maxSessions = 500
	// storeVersion 便于以后改结构时有地方判断；当前只写 1，读到任何值都按 1 解析。
	storeVersion = 1

	fileDevices  = "devices.json"
	fileSessions = "sessions.json"
)

// deviceRecord / sessionRecord 是落盘用的显式结构。
//
// 不能直接序列化 Device / Session：它们的 TenantID / UserID 打了 `json:"-"`（契约里
// 不能改），直接落盘会把归属丢掉，重启后所有设备变成公共资产。这里显式带上归属。
type deviceRecord struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenantId"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastSeenAt time.Time  `json:"lastSeenAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

type sessionRecord struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenantId"`
	UserID     string     `json:"userId"`
	DeviceID   string     `json:"deviceId"`
	Device     string     `json:"device,omitempty"`
	Mode       string     `json:"mode"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	EndedAt    *time.Time `json:"endedAt,omitempty"`
	EndReason  string     `json:"endReason,omitempty"`
	Client     string     `json:"client,omitempty"`
	Stats      Stats      `json:"stats"`
}

type deviceFile struct {
	Version int            `json:"version"`
	Items   []deviceRecord `json:"items"`
}

type sessionFile struct {
	Version int             `json:"version"`
	Items   []sessionRecord `json:"items"`
}

func (r deviceRecord) toDevice() Device {
	return Device{
		ID:         r.ID,
		TenantID:   r.TenantID,
		UserID:     r.UserID,
		Name:       r.Name,
		Platform:   r.Platform,
		CreatedAt:  r.CreatedAt,
		LastSeenAt: r.LastSeenAt,
		RevokedAt:  cloneTime(r.RevokedAt),
	}
}

func deviceToRecord(d *Device) deviceRecord {
	return deviceRecord{
		ID:         d.ID,
		TenantID:   d.TenantID,
		UserID:     d.UserID,
		Name:       d.Name,
		Platform:   d.Platform,
		CreatedAt:  d.CreatedAt,
		LastSeenAt: d.LastSeenAt,
		RevokedAt:  cloneTime(d.RevokedAt),
	}
}

func (r sessionRecord) toSession() Session {
	return Session{
		ID:         r.ID,
		TenantID:   r.TenantID,
		UserID:     r.UserID,
		DeviceID:   r.DeviceID,
		Device:     r.Device,
		Mode:       r.Mode,
		Status:     r.Status,
		CreatedAt:  r.CreatedAt,
		ExpiresAt:  r.ExpiresAt,
		ApprovedAt: cloneTime(r.ApprovedAt),
		EndedAt:    cloneTime(r.EndedAt),
		EndReason:  r.EndReason,
		Client:     r.Client,
		Stats:      r.Stats,
	}
}

func sessionToRecord(s *Session) sessionRecord {
	return sessionRecord{
		ID:         s.ID,
		TenantID:   s.TenantID,
		UserID:     s.UserID,
		DeviceID:   s.DeviceID,
		Device:     s.Device,
		Mode:       s.Mode,
		Status:     s.Status,
		CreatedAt:  s.CreatedAt,
		ExpiresAt:  s.ExpiresAt,
		ApprovedAt: cloneTime(s.ApprovedAt),
		EndedAt:    cloneTime(s.EndedAt),
		EndReason:  s.EndReason,
		Client:     s.Client,
		Stats:      s.Stats,
	}
}

// cloneTime 避免返回值与内部状态共享 *time.Time（外部改了会污染内存态）。
func cloneTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// loadJSON 读一个 JSON 文件。
// 文件不存在是正常的首启（返回 found=false）；存在但读不出来/解析失败一律报错。
func loadJSON(path string, into any) (found bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("remotedesk: 读取 %s 失败: %w", path, err)
	}
	if len(b) == 0 {
		return false, fmt.Errorf("remotedesk: %s 为空文件（不是正常首启，请人工确认后删除）", path)
	}
	if err := json.Unmarshal(b, into); err != nil {
		return false, fmt.Errorf("remotedesk: %s 已损坏，无法解析（数据不会自动丢弃，请人工检查或删除）: %w", path, err)
	}
	return true, nil
}

// writeJSONAtomic 原子落盘：临时文件 → fsync → rename，权限 0600。
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("remotedesk: 序列化 %s 失败: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("remotedesk: 创建目录 %s 失败: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".remote-*.tmp")
	if err != nil {
		return fmt.Errorf("remotedesk: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("remotedesk: 设置权限失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("remotedesk: 写 %s 失败: %w", filepath.Base(path), err)
	}
	// fsync：rename 只有元数据是原子的，数据页没落盘时断电可能留下空文件。
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("remotedesk: fsync %s 失败: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("remotedesk: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("remotedesk: 落盘 %s 失败: %w", filepath.Base(path), err)
	}
	// 目录项的 fsync 是 best-effort：某些文件系统（容器 overlay）不支持，失败不算错。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (m *manager) devicesPath() string  { return filepath.Join(m.dir, fileDevices) }
func (m *manager) sessionsPath() string { return filepath.Join(m.dir, fileSessions) }

// loadStore 启动时读盘。文件损坏要在这里就暴露，而不是等用户扫码配对时才发现。
func (m *manager) loadStore() error {
	var df deviceFile
	found, err := loadJSON(m.devicesPath(), &df)
	if err != nil {
		return err
	}
	if found {
		for _, r := range df.Items {
			if r.ID == "" {
				continue
			}
			d := r.toDevice()
			m.devices[d.ID] = &d
		}
	}

	var sf sessionFile
	found, err = loadJSON(m.sessionsPath(), &sf)
	if err != nil {
		return err
	}
	if found {
		for _, r := range sf.Items {
			if r.ID == "" {
				continue
			}
			s := r.toSession()
			// 会话是「带 TTL 的运行时状态」：进程重启后抓屏泵已经断了，
			// 假装它还是 active 会误导用户（点了没画面、还占着权限）。
			if s.Status == StatusActive || s.Status == StatusPending {
				now := time.Now()
				s.Status = StatusEnded
				s.EndedAt = &now
				s.EndReason = ReasonShutdown
			}
			m.sessions[s.ID] = &s
		}
	}
	return nil
}

func (m *manager) saveDevicesLocked() error {
	items := make([]deviceRecord, 0, len(m.devices))
	for _, d := range m.devices {
		items = append(items, deviceToRecord(d))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return writeJSONAtomic(m.devicesPath(), deviceFile{Version: storeVersion, Items: items})
}

func (m *manager) saveSessionsLocked() error {
	items := make([]sessionRecord, 0, len(m.sessions))
	for _, s := range m.sessions {
		items = append(items, sessionToRecord(s))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.Before(items[j].CreatedAt) })
	return writeJSONAtomic(m.sessionsPath(), sessionFile{Version: storeVersion, Items: items})
}

// saveDevices / saveSessions 自己拿锁，供已解锁的路径（Close、onFatal）调用。
func (m *manager) saveDevices() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveDevicesLocked()
}

func (m *manager) saveSessions() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveSessionsLocked()
}

// pruneSessionsLocked 把会话数压到 maxSessions。
// 先丢「已结束且最老」的：结束的会话只剩历史价值，而 active/pending 丢掉会让
// 用户看着看着突然 404，所以永远最后才动它们。
func (m *manager) pruneSessionsLocked() {
	if len(m.sessions) <= maxSessions {
		return
	}
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	for _, s := range list {
		if len(m.sessions) <= maxSessions {
			return
		}
		if isFinished(s.Status) {
			delete(m.sessions, s.ID)
		}
	}
	for _, s := range list {
		if len(m.sessions) <= maxSessions {
			return
		}
		delete(m.sessions, s.ID)
	}
}

func (m *manager) pruneDevicesLocked() {
	if len(m.devices) <= maxDevices {
		return
	}
	list := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].ID < list[j].ID
		}
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	for _, d := range list {
		if len(m.devices) <= maxDevices {
			return
		}
		if d.RevokedAt != nil {
			delete(m.devices, d.ID)
		}
	}
	for _, d := range list {
		if len(m.devices) <= maxDevices {
			return
		}
		delete(m.devices, d.ID)
	}
}

func isFinished(status string) bool {
	switch status {
	case StatusEnded, StatusExpired, StatusDenied:
		return true
	}
	return false
}
