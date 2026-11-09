package remotedesk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistenceRoundTripAndRestartEndsActiveSessions(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	env := r1OpenAt(t, dir, nil)
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	// 一条 view（active）+ 一条 control（pending）都会被重启收编成 ended。
	active, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeControl})
	if err != nil {
		t.Fatal(err)
	}
	// 一条用户主动结束的会话，重启后必须保留它原本的结束原因。
	// 用另一台设备，避免同设备同 mode 触发 superseded 把 active 那条顶掉。
	d2 := r1Pair(t, env.m, "t1", "u1", "Mac 2")
	done, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d2.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.EndSession(ctx, "t1", done.ID, ReasonUserEnded); err != nil {
		t.Fatal(err)
	}
	if err := env.m.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 落盘权限必须是 0600（同机其他用户读不到配对关系）。
	for _, name := range []string{"devices.json", "sessions.json"} {
		fi, err := os.Stat(filepath.Join(dir, "remote", name))
		if err != nil {
			t.Fatalf("%s 未落盘: %v", name, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s 权限应为 0600，实际 %04o", name, perm)
		}
	}

	env2 := r1OpenAt(t, dir, nil)
	// 归属（TenantID/UserID 在契约结构里是 json:"-"）必须靠落盘结构单独保存，
	// 否则重启后设备会变成「谁都能看见」。
	list, err := env2.m.ListDevices(ctx, "t1", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("重启后两台设备都应还在且归属正确，实际 %+v", list)
	}
	names := map[string]string{}
	for _, dv := range list {
		names[dv.ID] = dv.Name
	}
	if names[d.ID] != "Mac" || names[d2.ID] != "Mac 2" {
		t.Fatalf("重启后设备名对不上: %+v", names)
	}
	if list[0].Online {
		t.Fatal("重启后没有活动会话，不该显示在线")
	}

	got, err := env2.m.GetSession(ctx, "t1", active.ID)
	if err != nil {
		t.Fatalf("重启后会话应还在（作为历史）: %v", err)
	}
	if got.Status != StatusEnded || got.EndReason != ReasonShutdown || got.EndedAt == nil {
		t.Fatalf("重启后 active 会话必须标 ended/server_shutdown，实际 %s/%s", got.Status, got.EndReason)
	}
	gotPend, err := env2.m.GetSession(ctx, "t1", pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotPend.Status != StatusEnded || gotPend.EndReason != ReasonShutdown {
		t.Fatalf("重启后 pending 会话同样不可信，应标 ended，实际 %s/%s", gotPend.Status, gotPend.EndReason)
	}
	gotDone, err := env2.m.GetSession(ctx, "t1", done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDone.EndReason != ReasonUserEnded {
		t.Fatalf("原本已结束的会话不该被改写成 server_shutdown，实际 %s", gotDone.EndReason)
	}

	// 重启后设备可以直接用（不用重新配对）。
	if _, err := env2.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView}); err != nil {
		t.Fatalf("重启后应能直接用原设备开会话: %v", err)
	}
}

func TestCorruptStoreFailsOpen(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"devices.json", "{ 这不是 json"},
		{"sessions.json", "[1,2,3]"},
		{"devices.json", ""},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		rd := filepath.Join(dir, "remote")
		if err := os.MkdirAll(rd, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rd, tc.name), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		mgr, err := Open(r1Config(dir))
		if err == nil {
			_ = mgr.Close()
			t.Fatalf("%s 内容为 %q 时 Open 必须报错（数据丢了要让人知道）", tc.name, tc.content)
		}
		if !strings.Contains(err.Error(), "损坏") && !strings.Contains(err.Error(), "空文件") {
			t.Fatalf("错误信息应指明文件坏了，实际 %v", err)
		}
	}
}

func TestMissingStoreIsNormalFirstBoot(t *testing.T) {
	dir := t.TempDir()
	env := r1OpenAt(t, dir, nil)
	list, err := env.m.ListDevices(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("缺文件是正常首启，不该报错: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("首启不该有设备，实际 %d", len(list))
	}
}

func TestPruneSessionsDropsFinishedFirst(t *testing.T) {
	env := r1Open(t, nil)
	base := time.Now().Add(-time.Hour)

	env.m.mu.Lock()
	defer env.m.mu.Unlock()
	const extra = 10
	for i := 0; i < maxSessions+extra; i++ {
		id := fmt.Sprintf("sess_%04d", i)
		status := StatusEnded
		// 让最新的 extra 条是 active：淘汰必须优先挑「已结束且最老」，不能把活会话干掉。
		if i >= maxSessions {
			status = StatusActive
		}
		env.m.sessions[id] = &Session{
			ID:        id,
			TenantID:  "t1",
			Status:    status,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
	}
	env.m.pruneSessionsLocked()

	if len(env.m.sessions) != maxSessions {
		t.Fatalf("会话数应压到 %d，实际 %d", maxSessions, len(env.m.sessions))
	}
	for i := maxSessions; i < maxSessions+extra; i++ {
		id := fmt.Sprintf("sess_%04d", i)
		if _, ok := env.m.sessions[id]; !ok {
			t.Fatalf("active 会话 %s 不该被淘汰", id)
		}
	}
	for i := 0; i < extra; i++ {
		id := fmt.Sprintf("sess_%04d", i)
		if _, ok := env.m.sessions[id]; ok {
			t.Fatalf("最老的一批已结束会话该被淘汰，%s 还在", id)
		}
	}
}

func TestPruneDevicesDropsRevokedFirst(t *testing.T) {
	env := r1Open(t, nil)
	base := time.Now().Add(-time.Hour)

	env.m.mu.Lock()
	defer env.m.mu.Unlock()
	const extra = 7
	revoked := base
	for i := 0; i < maxDevices+extra; i++ {
		id := fmt.Sprintf("dev_%04d", i)
		d := &Device{ID: id, TenantID: "t1", UserID: "u1", CreatedAt: base.Add(time.Duration(i) * time.Second)}
		if i < extra {
			t := revoked
			d.RevokedAt = &t
		}
		env.m.devices[id] = d
	}
	env.m.pruneDevicesLocked()

	if len(env.m.devices) != maxDevices {
		t.Fatalf("设备数应压到 %d，实际 %d", maxDevices, len(env.m.devices))
	}
	for i := 0; i < extra; i++ {
		if _, ok := env.m.devices[fmt.Sprintf("dev_%04d", i)]; ok {
			t.Fatalf("已吊销的设备应优先淘汰，dev_%04d 还在", i)
		}
	}
}

func TestStoreRejectsTrailingGarbage(t *testing.T) {
	// 半截写入理论上被 rename 原子性挡住了，但人工编辑/磁盘故障仍可能留下坏文件。
	dir := t.TempDir()
	rd := filepath.Join(dir, "remote")
	if err := os.MkdirAll(rd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rd, "devices.json"), []byte(`{"version":1,"items":[]} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(r1Config(dir)); err == nil {
		t.Fatal("尾部有垃圾的 JSON 应报错，不该静默当成空列表")
	}
}

func TestEndSessionAfterCloseIsUnavailable(t *testing.T) {
	env := r1Open(t, nil)
	ctx := context.Background()
	d := r1Pair(t, env.m, "t1", "u1", "Mac")
	s, err := env.m.StartSession(ctx, StartRequest{TenantID: "t1", UserID: "u1", DeviceID: d.ID, Mode: ModeView})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := env.m.EndSession(ctx, "t1", s.ID, ReasonUserEnded); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Close 后 EndSession 应 ErrUnavailable，实际 %v", err)
	}
}
