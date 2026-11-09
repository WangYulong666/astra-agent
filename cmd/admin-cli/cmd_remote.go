package main

// 「手机看/控这台电脑」的 CLI。
//
// 这一组命令是给**人在电脑跟前**用的：生成配对码、看设备、批准接管、急停。
// 手机侧的画面与手势走 Web（WS + JPEG 帧），CLI 不碰——用命令行看画面没有意义，
// 但排障时「到底是缺屏幕录制权限还是辅助功能权限」必须能一行问出来，
// 那就是 remote capabilities 存在的理由。
//
// 三层能力是分开判断的，别写成「全有或全无」：
// 没有辅助功能权限时监看与快捷操作照常可用（只读模式），
// 所以只有「监看不可用」才返回 1，其它层不可用是降级、不是失败。
//
// 参数解析沿用三段式：splitCommand（锚点）→ clikit.SplitFlags（值型 flag
// 必须逐个声明，漏一个就会把值当成位置参数）→ flag.FlagSet。

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const remoteHelp = `用法：
  admin-cli remote capabilities [--strict]                    三层能力：监看 / 快捷操作 / 键鼠接管
  admin-cli remote pair [--device NAME]                       生成 6 位配对码（120 秒、一次性）
  admin-cli remote claim <code> [--device NAME]               用配对码把本机绑定成设备
  admin-cli remote devices                                    设备列表（在线 / 最后活跃 / 已吊销）
  admin-cli remote revoke <deviceID>                          吊销设备（级联断开其会话）
  admin-cli remote start --device ID [--mode view|control]    建会话，control 需桌面端确认
  admin-cli remote sessions [--limit 20]                      会话列表（状态 / 帧率 / 掉帧 / 时长）
  admin-cli remote approve <sessionID>                        批准接管（pending → active）
  admin-cli remote end <sessionID>                            急停：立即断开
  admin-cli remote action <sessionID> <动作> [参数]            快捷操作（不需要辅助功能权限）

动作：
  open-app <应用名>     打开 App（Finder / Terminal / 微信 ...）
  open-url <网址>       用默认浏览器打开，仅 http/https
  screenshot            抓一张图存到 <dataDir>/remote/shots
  lock                  锁屏 / 立即熄屏

时间一律按 UTC 显示（表头标注），否则本地时区会把「最后活跃」读错几小时。
所有命令都支持 --json 输出原始响应。
退出码：0 成功 · 1 执行失败（capabilities：监看不可用）· 2 用法错误。
`

// 所有带值的 flag 都必须列进来（含全局 server/token），否则值会被
// SplitFlags 当成位置参数。bool 型（--json / --strict / --yes / -v）不用列。
var remoteValueFlags = clikit.BoolFlags(
	"server", "token", "device", "mode", "limit",
)

// remote 的子命令名。与 logs 同理：只在「命令后紧跟一个非 flag 词」时
// splitCommand 才认子命令，`remote --json sessions` 里 sessions 会落进位置参数。
func isRemoteSub(s string) bool {
	switch s {
	case "capabilities", "caps", "pair", "pairing", "claim",
		"devices", "device", "revoke", "start", "sessions", "session",
		"approve", "end", "stop", "action":
		return true
	}
	return false
}

func (a *app) cmdRemote(sub string, args []string) int {
	if sub == "" {
		_, pos := clikit.SplitFlags(args, remoteValueFlags)
		if len(pos) > 0 && isRemoteSub(pos[0]) {
			sub = pos[0]
			for i, v := range args {
				if v == sub {
					args = append(append([]string{}, args[:i]...), args[i+1:]...)
					break
				}
			}
		}
	}
	switch sub {
	case "capabilities", "caps":
		return a.remoteCaps(args)
	case "pair", "pairing":
		return a.remotePair(args)
	case "claim":
		return a.remoteClaim(args)
	case "devices", "device":
		return a.remoteDevices(args)
	case "revoke":
		return a.remoteRevoke(args)
	case "start":
		return a.remoteStart(args)
	case "sessions", "session":
		return a.remoteSessions(args)
	case "approve":
		return a.remoteApprove(args)
	case "end", "stop":
		return a.remoteEnd(args)
	case "action":
		return a.remoteAction(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, remoteHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "remote 没有子命令 %q\n\n%s", sub, remoteHelp)
		return 2
	}
}

// ── 能力 ────────────────────────────────────────────────────────

// remoteCaps 把三层能力摊开成表格：能用的打 OK，不能用的说原因 + 修复路径。
//
// 退出码是这套命令里唯一有分支的地方：
//   - 监看不可用 → 1：没有画面，这个能力等于不存在；
//   - 快捷操作 / 键鼠接管不可用 → 0：只读模式是设计内的降级，不该让脚本红；
//   - --strict → 任意一层不可用都 1（给 CI / 部署自检用）。
func (a *app) remoteCaps(args []string) int {
	fs := a.newFlagSet("remote capabilities")
	strict := fs.Bool("strict", false, "任意一层不可用即返回 1")
	if _, code, done := a.parse(fs, args, remoteValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	caps, err := c.RemoteCapabilities(context.Background())
	if err != nil {
		return a.fail(err)
	}

	exit := 0
	degraded := !caps.Input.Available || !caps.Actions.Available
	if !caps.Capture.Available || (*strict && degraded) {
		exit = 1
	}

	if code := a.jsonOut(caps, func() error {
		fmt.Fprintf(a.out, "平台 %s", dash(caps.Platform))
		if caps.Screen.Width > 0 && caps.Screen.Height > 0 {
			fmt.Fprintf(a.out, " · 屏幕 %d×%d", caps.Screen.Width, caps.Screen.Height)
		}
		fmt.Fprintf(a.out, " · 帧率上限 %d fps\n\n", caps.MaxFPS)

		t := &clikit.Table{Header: []string{"层", "状态", "说明"}}
		type layer struct {
			name string
			cap  client.RemoteCapability
		}
		for _, l := range []layer{
			{"监看", caps.Capture},
			{"快捷操作", caps.Actions},
			{"键鼠接管", caps.Input},
		} {
			status, detail := "OK", dash(l.cap.Backend)
			if !l.cap.Available {
				status, detail = "不可用", dash(l.cap.Reason)
			}
			t.Rows = append(t.Rows, []string{l.name, status, detail})
			if !l.cap.Available && l.cap.Hint != "" {
				t.Rows = append(t.Rows, []string{"", "", "修复：" + l.cap.Hint})
			}
		}
		t.Render(a.out)

		if caps.Notice != "" {
			fmt.Fprintf(a.out, "\n%s\n", caps.Notice)
		}
		if !caps.Capture.Available {
			fmt.Fprintln(a.out, "\n监看不可用：手机端只会看到黑屏，先按上面的修复路径授权再重试。")
		} else if !caps.Input.Available {
			fmt.Fprintln(a.out, "\n只能看不能控（只读模式）：画面与快捷操作仍然可用。")
		}
		return nil
	}); code != 0 {
		return code
	}
	return exit
}

// ── 配对 ────────────────────────────────────────────────────────

func (a *app) remotePair(args []string) int {
	fs := a.newFlagSet("remote pair")
	device := fs.String("device", "", "设备名，留空取电脑 hostname")
	if _, code, done := a.parse(fs, args, remoteValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	pair, err := c.RemotePair(context.Background(), *device)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(pair, func() error {
		// 剩余秒数在这里现算：服务端给的是绝对时间，用户要看的是「还有多久」。
		left := int(pair.Remaining(time.Now()).Seconds())
		fmt.Fprintf(a.out, "配对码    %s\n", pair.Code)
		fmt.Fprintf(a.out, "剩余      %d 秒（%s 到期）\n", left, formatTime(pair.ExpiresAt))
		if *device != "" {
			fmt.Fprintf(a.out, "设备名    %s\n", *device)
		}
		// payload 单独一行、不加别的字：它会被人选中复制，也会被前端画成二维码。
		fmt.Fprintf(a.out, "payload   %s\n", dash(pair.Payload))
		if pair.DeepLink != "" {
			fmt.Fprintf(a.out, "深链      %s\n", pair.DeepLink)
		}
		fmt.Fprintln(a.out, "\n在手机控制面「远程控制 → 添加电脑」里输入这 6 位码；只能用一次，过期重新生成。")
		return nil
	})
}

func (a *app) remoteClaim(args []string) int {
	fs := a.newFlagSet("remote claim")
	device := fs.String("device", "", "设备名，留空取本机 hostname")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli remote claim <code> [--device NAME]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	dev, err := c.RemoteClaim(context.Background(), pos[0], *device)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(dev, func() error {
		fmt.Fprintf(a.out, "已绑定设备 %s\n", dash(dev.ID))
		t := &clikit.Table{Header: []string{"设备ID", "名称", "平台", "状态", "绑定时间 (UTC)"}}
		t.Rows = append(t.Rows, []string{
			dash(dev.ID), dash(dev.Name), dash(dev.Platform),
			remoteDeviceState(dev), formatTime(dev.CreatedAt),
		})
		t.Render(a.out)
		return nil
	})
}

// ── 设备 ────────────────────────────────────────────────────────

func (a *app) remoteDevices(args []string) int {
	fs := a.newFlagSet("remote devices")
	if _, code, done := a.parse(fs, args, remoteValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.RemoteDevices(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{Header: []string{"设备ID", "名称", "平台", "状态", "最后活跃 (UTC)"}}
		online := 0
		for _, d := range items {
			if d.Online && d.RevokedAt == nil {
				online++
			}
			t.Rows = append(t.Rows, []string{
				dash(d.ID), dash(d.Name), dash(d.Platform),
				remoteDeviceState(d), formatTime(d.LastSeenAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 台设备 · 在线 %d 台\n", len(items), online)
		return nil
	})
}

// remoteDeviceState 把 (已吊销, 在线) 两个布尔压成一个状态词。
// 已吊销优先：一台吊销掉的设备即使还有残留会话，也不该显示成「在线」。
func remoteDeviceState(d client.RemoteDevice) string {
	switch {
	case d.RevokedAt != nil:
		return "已吊销"
	case d.Online:
		return "在线"
	default:
		return "离线"
	}
}

func (a *app) remoteRevoke(args []string) int {
	fs := a.newFlagSet("remote revoke")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli remote revoke <deviceID> [--yes]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if !a.confirm(fmt.Sprintf("吊销设备 %s 后，该设备上的活动会话会立即断开，继续？", pos[0])) {
		fmt.Fprintln(a.out, "已取消")
		return 1
	}
	if err := c.RemoteRevokeDevice(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	return a.jsonOut(map[string]any{"id": pos[0], "revoked": true}, func() error {
		fmt.Fprintf(a.out, "已吊销设备 %s，其活动会话已断开\n", pos[0])
		return nil
	})
}

// ── 会话 ────────────────────────────────────────────────────────

func (a *app) remoteStart(args []string) int {
	fs := a.newFlagSet("remote start")
	device := fs.String("device", "", "设备 ID（remote devices 可查）")
	mode := fs.String("mode", client.RemoteModeView, "view=只看画面，control=可接管键鼠")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	// 位置参数也当设备 id，省掉打 --device 的九个字符。
	if *device == "" && len(pos) > 0 {
		*device = pos[0]
	}
	if *device == "" {
		return a.fail(&client.UseError{Msg: "缺少设备 id\n用法：admin-cli remote start --device ID [--mode view|control]"})
	}
	// 不用 switch 校验收值：服务端也会挡，这里只是省一次往返，
	// 顺带保证 --json 场景下错也是退出码 2 而不是 1。
	if !remoteModes[*mode] {
		return a.fail(&client.UseError{Msg: "mode 只能是 view 或 control，收到 " + strconv.Quote(*mode)})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	sess, err := c.RemoteStartSession(context.Background(), *device, *mode)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(sess, func() error {
		fmt.Fprintf(a.out, "会话      %s\n", dash(sess.ID))
		fmt.Fprintf(a.out, "设备      %s（%s）\n", dash(sess.Device), dash(sess.DeviceID))
		fmt.Fprintf(a.out, "模式      %s\n", dash(sess.Mode))
		fmt.Fprintf(a.out, "状态      %s\n", dash(sess.Status))
		fmt.Fprintf(a.out, "到期      %s\n", formatTime(sess.ExpiresAt))
		fmt.Fprintf(a.out, "建会话    %s\n", formatTime(sess.CreatedAt))
		switch {
		case sess.Status == client.RemoteStatusPending:
			fmt.Fprintf(a.out, "\n等待桌面确认：在电脑上打开控制面「远程控制」面板点「允许」，"+
				"或在本机执行 admin-cli remote approve %s。批准前手机只能看画面。\n", sess.ID)
		case sess.Mode == client.RemoteModeControl:
			fmt.Fprintf(a.out, "\n已进入可接管模式（本机策略不要求二次确认）。"+
				"不放心就随时 admin-cli remote end %s。\n", sess.ID)
		}
		return nil
	})
}

// remoteModes 是合法模式集合。用 map 而不是 switch：这是数据校验，
// 不是命令分派（后者才需要登记进 clikit 的命令表）。
var remoteModes = map[string]bool{
	client.RemoteModeView:    true,
	client.RemoteModeControl: true,
}

func (a *app) remoteSessions(args []string) int {
	fs := a.newFlagSet("remote sessions")
	limit := fs.Int("limit", 20, "返回条数")
	if _, code, done := a.parse(fs, args, remoteValueFlags); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.RemoteSessions(context.Background(), *limit)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := &clikit.Table{
			Header: []string{"会话ID", "设备", "模式", "状态", "帧率", "掉帧", "时长", "创建 (UTC)"},
			Right:  map[int]bool{4: true, 5: true, 6: true},
		}
		active := 0
		now := time.Now()
		for _, s := range items {
			if s.Status == client.RemoteStatusActive {
				active++
			}
			t.Rows = append(t.Rows, []string{
				dash(s.ID), dash(s.Device), dash(s.Mode), dash(s.Status),
				fmt.Sprintf("%.1f", s.Stats.FPS), strconv.FormatUint(s.Stats.Dropped, 10),
				remoteDuration(remoteElapsed(s, now)), formatTime(s.CreatedAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "共 %d 路会话 · 进行中 %d 路\n", len(items), active)
		return nil
	})
}

// remoteElapsed 算这路会话活了多久：已结束的看 EndedAt，还在跑的看现在。
func remoteElapsed(s client.RemoteSession, now time.Time) time.Duration {
	end := now
	if s.EndedAt != nil {
		end = *s.EndedAt
	}
	if end.Before(s.CreatedAt) {
		return 0
	}
	return end.Sub(s.CreatedAt)
}

// remoteDuration 把时长压成 3m12s / 1h02m 这种短格式：
// 表格里要的是量级，不是纳秒。
func remoteDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func (a *app) remoteApprove(args []string) int {
	fs := a.newFlagSet("remote approve")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli remote approve <sessionID>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	sess, err := c.RemoteApprove(context.Background(), pos[0])
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(sess, func() error {
		fmt.Fprintf(a.out, "会话 %s 已批准 · 状态 %s", dash(sess.ID), dash(sess.Status))
		if sess.ApprovedAt != nil {
			fmt.Fprintf(a.out, " · 批准于 %s", formatTime(*sess.ApprovedAt))
		}
		fmt.Fprintln(a.out)
		return nil
	})
}

func (a *app) remoteEnd(args []string) int {
	fs := a.newFlagSet("remote end")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli remote end <sessionID>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	// 急停不做二次确认：它存在的场景就是「现在必须断」。
	if err := c.RemoteEndSession(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	return a.jsonOut(map[string]any{"id": pos[0], "ended": true}, func() error {
		fmt.Fprintf(a.out, "已断开会话 %s\n", pos[0])
		return nil
	})
}

// ── 快捷操作 ────────────────────────────────────────────────────

// remoteActionNames 是「命令行写法 → 协议动作名」的映射。
// 命令行用连字符（open-app），协议用下划线（open_app）；两份都认短别名。
// 用 map 不用 switch：这不是命令分派，不进 clikit 命令表。
var remoteActionNames = map[string]string{
	"open-app":   client.RemoteActionOpenApp,
	"app":        client.RemoteActionOpenApp,
	"open-url":   client.RemoteActionOpenURL,
	"url":        client.RemoteActionOpenURL,
	"screenshot": client.RemoteActionScreenshot,
	"shot":       client.RemoteActionScreenshot,
	"lock":       client.RemoteActionLock,
}

const remoteActionUsage = `用法：admin-cli remote action <sessionID> <动作> [参数]

动作：
  open-app <应用名>     如 open-app Finder
  open-url <网址>       仅 http/https，如 open-url https://example.com
  screenshot            返回服务端落盘路径
  lock                  锁屏 / 立即熄屏`

func (a *app) remoteAction(args []string) int {
	fs := a.newFlagSet("remote action")
	pos, code, done := a.parse(fs, args, remoteValueFlags)
	if done {
		return code
	}
	if err := a.needArgs(pos, 2, "admin-cli remote action <sessionID> <open-app|open-url|screenshot|lock> [参数]"); err != nil {
		return a.fail(err)
	}
	action, ok := remoteActionNames[pos[1]]
	if !ok {
		return a.fail(&client.UseError{Msg: "未知动作 " + strconv.Quote(pos[1]) + "\n" + remoteActionUsage})
	}
	var argsMap map[string]string
	switch action {
	case client.RemoteActionOpenApp:
		if len(pos) < 3 {
			return a.fail(&client.UseError{Msg: "open-app 缺少应用名\n" + remoteActionUsage})
		}
		argsMap = map[string]string{"name": pos[2]}
	case client.RemoteActionOpenURL:
		if len(pos) < 3 {
			return a.fail(&client.UseError{Msg: "open-url 缺少网址\n" + remoteActionUsage})
		}
		argsMap = map[string]string{"url": pos[2]}
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	res, err := c.RemoteAction(context.Background(), pos[0], action, argsMap)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(res, func() error {
		state := "失败"
		if res.OK {
			state = "成功"
		}
		fmt.Fprintf(a.out, "%s · %s", dash(res.Action), state)
		if res.Detail != "" {
			fmt.Fprintf(a.out, " · %s", res.Detail)
		}
		fmt.Fprintln(a.out)
		if res.Path != "" {
			fmt.Fprintf(a.out, "截图路径 %s\n", res.Path)
		}
		return nil
	})
}
