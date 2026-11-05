package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AppleContainer 用 macOS 26 的 `container` CLI（apple/container）做沙箱后端。
//
// 为什么选它而不是 Docker Desktop / colima：
//   - 每个容器跑在自己的轻量 Linux VM 里（Virtualization.framework），内核独立，
//     这才是「沙箱」而非「共享内核的隔离」——与 CubeSandbox 的 microVM 模型同构
//   - 冷启动 <350ms，没有常驻 daemon 吃内存（本机 16GB，Docker Desktop 那套太重）
//   - 官方支持 macOS 26 + Apple Silicon，正是本机环境
//
// 与生产的差异（跨 provider 契约里必须写清的部分）：
//   - **CPU 架构**：Mac 是 arm64，镜像必须 arm64。生产的 CubeSandbox 是 x86_64。
//     所以本地跑通不代表 x64 镜像没问题，反之亦然。镜像构建要按 TARGETARCH 分架构。
//   - Snapshot 走镜像 commit 语义不同，这里明确返回 ErrUnsupported，不做假装。
//
// 前置条件：装 apple/container 的签名 pkg 并 `container system start`。没装时
// Create 会返回可直接照做的提示，而不是一个壳错误。
type AppleContainer struct {
	opts Options
}

// NewAppleContainer 构造 provider。
func NewAppleContainer(opts Options) *AppleContainer { return &AppleContainer{opts: opts} }

func (a *AppleContainer) Name() string { return "applecontainer" }

// containerName 把沙箱 ID 映射成容器名。加前缀避免和机器上别的容器混。
func (a *AppleContainer) containerName(id string) string { return "astra-" + id }

// CheckReady 检查 container CLI 与系统服务是否就绪。
// 单独暴露出来，好让 CLI 在 `doctor` 里一次性把所有缺失项列给用户。
func (a *AppleContainer) CheckReady(ctx context.Context) error {
	if _, err := exec.LookPath("container"); err != nil {
		return errors.New(
			"未安装 Apple container。macOS 26 下装它：\n" +
				"  1) 从 https://github.com/apple/container/releases 下载签名 .pkg\n" +
				"  2) 双击安装（需管理员密码，会装到 /usr/local）\n" +
				"  3) container system start\n" +
				"（不想装也能开发：用 -provider local）")
	}
	out, err := exec.CommandContext(ctx, "container", "system", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("container 系统服务未启动，先跑 `container system start`：%s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *AppleContainer) Create(ctx context.Context, req CreateRequest) (Sandbox, error) {
	if err := validateID(req.ID); err != nil {
		return nil, err
	}
	if err := a.CheckReady(ctx); err != nil {
		return nil, err
	}

	name := a.containerName(req.ID)

	// 已存在则复用（幂等）。存在但停了就 start。
	if a.exists(ctx, name) {
		if !a.running(ctx, name) {
			if out, err := exec.CommandContext(ctx, "container", "start", name).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("sandbox: 启动已有容器 %s 失败: %s", name, strings.TrimSpace(string(out)))
			}
		}
		port, err := a.publishedPort(ctx, name)
		if err != nil {
			return nil, err
		}
		return &appleSandbox{p: a, id: req.ID, name: name, port: port, token: reqToken(req)}, nil
	}

	if req.Template == "" {
		req.Template = a.opts.ProxyImage
	}
	if req.Template == "" {
		req.Template = "astra/agent-dsh:dev"
	}

	port, err := pickFreePort(43100)
	if err != nil {
		return nil, err
	}

	args := []string{
		"run", "-d",
		"--name", name,
		// 只绑 127.0.0.1：acp-proxy 是给本机 worker 连的，不该暴露到局域网
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		"-e", "ASTRA_PROXY_TOKEN=" + reqToken(req),
		"-e", "DSH_HOME=/var/lib/dsh",
	}
	if req.Workspace != "" {
		args = append(args, "-v", req.Workspace+":/workspace")
	}
	if req.Timeout > 0 {
		// container CLI 没有统一的超时参数，交给调用方在外部管；这里只记录意图
		args = append(args, "-e", fmt.Sprintf("ASTRA_TIMEOUT=%s", req.Timeout))
	}
	args = append(args, req.Template)

	if out, err := exec.CommandContext(ctx, "container", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("sandbox: container run 失败: %s", strings.TrimSpace(string(out)))
	}

	if err := waitHTTP(ctx, port, 60*time.Second); err != nil {
		_, _ = exec.CommandContext(ctx, "container", "delete", "--force", name).CombinedOutput()
		return nil, fmt.Errorf("sandbox: 容器起来了但 acp-proxy 未就绪（端口 %d）：%w", port, err)
	}

	return &appleSandbox{p: a, id: req.ID, name: name, port: port, token: reqToken(req)}, nil
}

func (a *AppleContainer) exists(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "container", "list", "--all", "--format", "json").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"`+name+`"`)
}

func (a *AppleContainer) running(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "container", "list", "--format", "json").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), `"`+name+`"`)
}

var portRe = regexp.MustCompile(`127\.0\.0\.1:(\d+)->8080`)

// publishedPort 从 `container list` 里把宿主机端口读回来——重启后端口可能变，
// 不能只依赖内存里的值。
func (a *AppleContainer) publishedPort(ctx context.Context, name string) (int, error) {
	out, err := exec.CommandContext(ctx, "container", "list", "--all").CombinedOutput()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, name) {
			continue
		}
		if m := portRe.FindStringSubmatch(line); m != nil {
			return strconv.Atoi(m[1])
		}
	}
	return 0, fmt.Errorf("sandbox: 找不到容器 %s 的宿主机端口", name)
}

// readToken 从容器环境里把 token 读回来。
// 不另外落盘是为了让容器自己成为凭据的唯一来源——容器没了 token 也就没意义了。
func (a *AppleContainer) readToken(ctx context.Context, name string) string {
	out, err := exec.CommandContext(ctx, "container", "exec", name, "--", "printenv", "ASTRA_PROXY_TOKEN").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (a *AppleContainer) Acquire(ctx context.Context, id string) (Sandbox, error) {
	name := a.containerName(id)
	if !a.exists(ctx, name) {
		return nil, ErrNotFound
	}
	if !a.running(ctx, name) {
		return nil, ErrNotRunning
	}
	port, err := a.publishedPort(ctx, name)
	if err != nil {
		return nil, err
	}
	return &appleSandbox{p: a, id: id, name: name, port: port, token: a.readToken(ctx, name)}, nil
}

// Destroy 强制删除容器，不要求它处于运行态。
func (a *AppleContainer) Destroy(ctx context.Context, id string) error {
	if err := validateID(id); err != nil {
		return err
	}
	name := a.containerName(id)
	if !a.exists(ctx, name) {
		return ErrNotFound
	}
	if out, err := exec.CommandContext(ctx, "container", "delete", "--force", name).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: 删除容器 %s 失败: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *AppleContainer) List(ctx context.Context) ([]Info, error) {
	out, err := exec.CommandContext(ctx, "container", "list", "--all").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var infos []Info
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "astra-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "astra-") {
			continue
		}
		id := strings.TrimPrefix(fields[0], "astra-")
		state := StateUnknown
		if strings.Contains(line, "running") {
			state = StateRunning
		} else if strings.Contains(line, "stopped") {
			state = StateStopped
		}
		port := 0
		if m := portRe.FindStringSubmatch(line); m != nil {
			port, _ = strconv.Atoi(m[1])
		}
		ep := Endpoint{}
		if port > 0 {
			ep.URL = fmt.Sprintf("ws://127.0.0.1:%d/acp", port)
		}
		infos = append(infos, Info{
			ID: id, Provider: "applecontainer", State: state,
			Endpoint: ep, Detail: "container=" + fields[0],
		})
	}
	return infos, nil
}

type appleSandbox struct {
	p     *AppleContainer
	id    string
	name  string
	port  int
	token string
}

func (s *appleSandbox) ID() string { return s.id }

func (s *appleSandbox) Endpoint() Endpoint {
	return Endpoint{URL: fmt.Sprintf("ws://127.0.0.1:%d/acp", s.port), Token: s.token}
}

func (s *appleSandbox) Info(ctx context.Context) (Info, error) {
	state := StateStopped
	if s.p.running(ctx, s.name) {
		state = StateRunning
	}
	return Info{
		ID: s.id, Provider: "applecontainer", State: state,
		Endpoint: s.Endpoint(), Detail: "container=" + s.name,
	}, nil
}

func (s *appleSandbox) Exec(ctx context.Context, cmdline []string, opts ExecOpts) (ExecResult, error) {
	if len(cmdline) == 0 {
		return ExecResult{}, errors.New("sandbox: 空命令")
	}
	args := []string{"exec"}
	if opts.Dir != "" {
		args = append(args, "-w", opts.Dir)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, s.name, "--")
	args = append(args, cmdline...)

	cmd := exec.CommandContext(ctx, "container", args...)
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	res := ExecResult{Stdout: out, Stderr: []byte(errb.String())}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// Snapshot 不支持。Apple container 1.0 没有 docker commit 那种「容器转镜像」，
// 硬凑只能靠 `container build` 重新构建，语义与「冻结当前状态」完全不同。
// 与其做半吊子实现，不如明确拒绝，让调用方回退到 template 重建。
func (s *appleSandbox) Snapshot(ctx context.Context, name string) (string, error) {
	return "", fmt.Errorf("%w: applecontainer 无 snapshot；请用 template 重建，或改用 local（APFS clonefile）",
		ErrUnsupported)
}

func (s *appleSandbox) Pause(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "container", "stop", s.name).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: stop 失败: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *appleSandbox) Resume(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "container", "start", s.name).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: start 失败: %s", strings.TrimSpace(string(out)))
	}
	return waitHTTP(ctx, s.port, 45*time.Second)
}

func (s *appleSandbox) Close() error { return nil }

func (s *appleSandbox) Kill(ctx context.Context) error {
	_, err := exec.CommandContext(ctx, "container", "delete", "--force", s.name).CombinedOutput()
	return err
}

var _ Provider = (*AppleContainer)(nil)
var _ Sandbox = (*appleSandbox)(nil)
