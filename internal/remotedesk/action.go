package remotedesk

// 快捷操作（L2）：打开 App / 打开网址 / 截屏存盘 / 锁屏。
//
// 这一层**不需要辅助功能权限**，所以「缺 TCC 就只能干看着」时它仍然可用，
// 是救急路径里性价比最高的一层（电脑弹了个框、服务没起、想锁屏走人）。
//
// 注入防护：所有命令都走 exec.Command 传参（不拼 shell 字符串），
// 并且在执行前对参数做白名单校验——即便将来有人把 exec 换成 sh -c，
// 元字符也进不来。两个独立防线，任何一条单独失效都不会变成 RCE。

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// execCommand 抽成变量，测试可替换以校验「参数是怎么传下去的」，
// 生产环境恒为 exec.CommandContext（带 ctx 超时/取消，不会挂死）。
var execCommand = exec.CommandContext

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := execCommand(ctx, name, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		if detail == "" {
			return fmt.Errorf("执行 %s 失败: %w", name, err)
		}
		return fmt.Errorf("执行 %s 失败: %w（%s）", name, err, detail)
	}
	return nil
}

// 应用名里明确禁止的字符：shell 元字符、引号、重定向、通配、路径分隔符、控制字符。
//
// 为什么连 `/` 一起禁：`open -a /tmp/evil.app` 能启动任意位置的 bundle，
// 而快捷操作只需要「按名字打开已安装的 App」，没有传路径的正当场景。
const forbiddenAppName = "`$;|&<>\"'\\/*?!#~^(){}[]%\n\r\t\x00"

// validateAppName 是白名单式校验：只接受「看起来像 App 名字」的字符串。
func validateAppName(name string) (string, error) {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "", fmt.Errorf("%w: 缺少应用名", ErrDenied)
	case strings.HasPrefix(n, "-"):
		// `open -a -g` 这类会把选项当参数，属于参数注入。
		return "", fmt.Errorf("%w: 应用名不能以 - 开头", ErrDenied)
	case len([]rune(n)) > 128:
		return "", fmt.Errorf("%w: 应用名过长", ErrDenied)
	}
	if i := strings.IndexAny(n, forbiddenAppName); i >= 0 {
		return "", fmt.Errorf("%w: 应用名包含非法字符 %q", ErrDenied, n[i])
	}
	return n, nil
}

// validateURL 只放行 http/https。
// 挡的是 file://（本地文件读取）、javascript:/data:（前端注入）、以及各种
// 自定义 scheme（`open` 会把它交给任意注册了该 scheme 的 App，等于间接执行）。
func validateURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w: 缺少 url", ErrDenied)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w: url 解析失败: %v", ErrDenied, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: 只允许 http/https，收到 %q", ErrDenied, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: url 缺少主机名", ErrDenied)
	}
	return u.String(), nil
}

func (m *manager) RunAction(ctx context.Context, req ActionRequest) (ActionResult, error) {
	res := ActionResult{Action: req.Action}
	if err := m.ensureOpen(); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	m.mu.RLock()
	s, ok := m.sessions[req.SessionID]
	var snap Session
	if ok {
		snap = cloneSession(s)
	}
	m.mu.RUnlock()

	// 归属：跨租户一律「不存在」。用户维度只在双方都有值时才比，
	// 兼容单机模式（principal 里 user 为空）与多租户模式。
	if !ok || snap.TenantID != req.TenantID {
		return res, ErrNotFound
	}
	if req.UserID != "" && snap.UserID != "" && snap.UserID != req.UserID {
		return res, ErrNotFound
	}
	if snap.Status != StatusActive {
		// 已结束的当不存在；还在等批准的明确说「没批准不能动手」。
		if isFinished(snap.Status) {
			return res, ErrNotFound
		}
		return res, fmt.Errorf("%w: 会话状态 %s，不允许执行快捷操作", ErrDenied, snap.Status)
	}

	err := m.doAction(ctx, &res, req.Args)
	res.OK = err == nil
	// 只记动作名，不记参数内容（url 可能带 query 里的敏感值，App 名也可能含人名）。
	m.emitAudit("remote.action", req.SessionID, map[string]any{
		"action": req.Action,
		"ok":     err == nil,
		"device": snap.DeviceID,
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

func (m *manager) doAction(ctx context.Context, res *ActionResult, args map[string]string) error {
	switch res.Action {
	case ActionOpenApp:
		return m.actionOpenApp(ctx, res, args["name"])
	case ActionOpenURL:
		return m.actionOpenURL(ctx, res, args["url"])
	case ActionScreenshot:
		return m.actionScreenshot(ctx, res)
	case ActionLock:
		return m.actionLock(ctx)
	default:
		return fmt.Errorf("%w: 未知快捷操作 %q", ErrDenied, res.Action)
	}
}

func (m *manager) actionOpenApp(ctx context.Context, res *ActionResult, name string) error {
	app, err := validateAppName(name)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		// 参数分开传：即使 app 里带空格/标点，也只会是 `open` 的一个 argv。
		if err := runCommand(ctx, "open", "-a", app); err != nil {
			return fmt.Errorf("%w: 打开应用失败: %v", ErrUnavailable, err)
		}
	case "linux":
		if _, err := exec.LookPath("gtk-launch"); err != nil {
			return fmt.Errorf("%w: 未找到 gtk-launch，无法按名字启动应用（可改用 open_url）", ErrUnavailable)
		}
		if err := runCommand(ctx, "gtk-launch", app); err != nil {
			return fmt.Errorf("%w: 打开应用失败: %v", ErrUnavailable, err)
		}
	default:
		return unsupportedAction(res.Action)
	}
	res.Detail = "已打开 " + app
	return nil
}

func (m *manager) actionOpenURL(ctx context.Context, res *ActionResult, raw string) error {
	u, err := validateURL(raw)
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		if err := runCommand(ctx, "open", u); err != nil {
			return fmt.Errorf("%w: 打开网址失败: %v", ErrUnavailable, err)
		}
	case "linux":
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return fmt.Errorf("%w: 未找到 xdg-open", ErrUnavailable)
		}
		if err := runCommand(ctx, "xdg-open", u); err != nil {
			return fmt.Errorf("%w: 打开网址失败: %v", ErrUnavailable, err)
		}
	default:
		return unsupportedAction(res.Action)
	}
	res.Detail = "已在桌面浏览器打开"
	return nil
}

func (m *manager) actionScreenshot(ctx context.Context, res *ActionResult) error {
	c, err := m.sharedCapture()
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, err.Error())
	}
	if info := c.Info(); !info.Available {
		reason := info.Reason
		if reason == "" {
			reason = "抓屏后端不可用"
		}
		return fmt.Errorf("%w: %s", ErrUnavailable, reason)
	}
	f, err := c.Grab(ctx, GrabOptions{MaxWidth: m.pol.MaxWidth, Quality: m.pol.Quality})
	if err != nil {
		return fmt.Errorf("%w: 抓屏失败: %v", ErrUnavailable, err)
	}
	dir := filepath.Join(m.dir, "shots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: 创建截图目录失败: %v", ErrUnavailable, err)
	}
	// 毫秒精度（Go 只认 ".000" 这种带小数点的写法）：连点两次不会互相覆盖。
	name := time.Now().Format("20060102-150405.000") + ".jpg"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, f.JPEG, 0o600); err != nil {
		return fmt.Errorf("%w: 写截图失败: %v", ErrUnavailable, err)
	}
	res.Path = path
	res.Detail = fmt.Sprintf("%dx%d，%d 字节", f.Width, f.Height, len(f.JPEG))
	return nil
}

func (m *manager) actionLock(ctx context.Context) error {
	switch runtime.GOOS {
	case "darwin":
		// displaysleepnow 立即熄屏（等价于锁屏），不依赖 AppleScript 权限。
		if err := runCommand(ctx, "pmset", "displaysleepnow"); err != nil {
			return fmt.Errorf("%w: 锁屏失败: %v", ErrUnavailable, err)
		}
	case "linux":
		if _, err := exec.LookPath("loginctl"); err != nil {
			return fmt.Errorf("%w: 未找到 loginctl", ErrUnavailable)
		}
		if err := runCommand(ctx, "loginctl", "lock-session"); err != nil {
			return fmt.Errorf("%w: 锁屏失败: %v", ErrUnavailable, err)
		}
	default:
		return unsupportedAction(ActionLock)
	}
	return nil
}

func unsupportedAction(action string) error {
	return fmt.Errorf("%w: 平台 %s 暂不支持 %s", ErrUnavailable, runtime.GOOS, action)
}

// actionsCapability 判断 L2 是否可用：它只依赖系统自带的 open/pmset/xdg-open，
// 不依赖 TCC 权限，所以在绝大多数机器上都是 true。
func actionsCapability() Capability {
	switch runtime.GOOS {
	case "darwin":
		return Capability{Available: true, Backend: "open+pmset"}
	case "linux":
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return Capability{
				Available: false,
				Reason:    "未找到 xdg-open",
				Hint:      "安装 xdg-utils（Debian/Ubuntu: apt install xdg-utils）",
			}
		}
		return Capability{Available: true, Backend: "xdg-open+loginctl"}
	default:
		return Capability{
			Available: false,
			Reason:    "平台 " + runtime.GOOS + " 未实现快捷操作",
			Hint:      "目前只支持 macOS / Linux",
		}
	}
}
