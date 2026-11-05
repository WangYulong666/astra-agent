package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

// cmdLogin 两种登录方式：
//   - --token：已有 token（API key 或别处复制来的 JWT），只做一次验证；
//   - --email/--password：走 /v1/auth/login 换 token。
//
// 无论哪种，落盘前都要真的打一次 /v1/meta 验证——否则用户会以为登录成功，
// 直到下一个命令才看到 401，那种「保存了一个坏 token」的状态最难排查。
func (a *app) cmdLogin(args []string) int {
	fs := a.newFlagSet("login")
	email := fs.String("email", "", "邮箱（配合 --password 走登录接口换 token）")
	password := fs.String("password", "", "密码")
	tenant := fs.String("tenant", "default", "租户 slug")
	as := fs.String("as", "", "只写进配置用于提示身份（user/admin），不参与鉴权")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "email", "password", "tenant", "as"))
	if done {
		return code
	}
	_ = pos

	server := a.g.server
	if server == "" {
		server = a.cfg.Server
	}
	if server == "" {
		return a.fail(&client.UseError{Msg: "缺少 --server\n用法：astra-cli login --server http://127.0.0.1:7788 --token <token>"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token := a.g.token
	if token == "" {
		token = a.cfg.Token
	}
	loginEmail := *email
	if token != "" && loginEmail == "" && *password == "" {
		// 用已有 token 登录：验证一下还能不能用。
		c := client.New(server, token)
		if a.g.verbose {
			c.SetLogger(func(f string, xs ...any) { fmt.Fprintf(a.err, "[v] "+f+"\n", xs...) })
		}
		meta, err := c.Meta(ctx)
		if err != nil {
			return a.fail(fmt.Errorf("这个 token 用不了：%w", err))
		}
		loginEmail = meta["email"]
	} else {
		if *email == "" || *password == "" {
			return a.fail(&client.UseError{Msg: "需要 --token，或者 --email 与 --password 同时提供\n用法：astra-cli login --server URL --token T\n      astra-cli login --server URL --email E --password P [--tenant default]"})
		}
		c := client.New(server, "")
		res, err := c.Login(ctx, *tenant, *email, *password)
		if err != nil {
			return a.fail(err)
		}
		token = res.Token
		loginEmail = res.User.Email
		if res.Tenant.Slug != "" {
			*tenant = res.Tenant.Slug
		}
	}

	cfg := clikit.Config{
		Server: server,
		Token:  token,
		Email:  loginEmail,
		Tenant: *tenant,
		Role:   *as,
	}
	if a.cfgPath == "" {
		return a.fail(&client.UseError{Msg: "无法定位配置目录（$HOME 不可写？）"})
	}
	if err := clikit.SaveConfig(a.cfgPath, cfg); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		_ = clikit.PrintJSON(a.out, cfg)
		return 0
	}
	fmt.Fprintf(a.out, "已登录 %s\n", server)
	if loginEmail != "" {
		fmt.Fprintf(a.out, "  账号：%s\n", loginEmail)
	}
	if *tenant != "" {
		fmt.Fprintf(a.out, "  租户：%s\n", *tenant)
	}
	fmt.Fprintf(a.out, "  凭据已保存到 %s（0600）\n", a.cfgPath)
	return 0
}

// cmdLogout 删本地配置。没有服务端调用：JWT 是无状态的，
// 「登出」对本地 CLI 而言就是忘掉凭据。
func (a *app) cmdLogout(args []string) int {
	fs := a.newFlagSet("logout")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	if a.cfgPath == "" {
		return a.fail(&client.UseError{Msg: "无法定位配置目录"})
	}
	if err := os.Remove(a.cfgPath); err != nil && !os.IsNotExist(err) {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "已删除本地凭据：%s\n", a.cfgPath)
	return 0
}

// cmdWhoami 打印当前身份 + 控制面能力开关。
// 排障时第一条要跑的命令：它同时回答「token 是谁」「连的是哪个服务」。
func (a *app) cmdWhoami(args []string) int {
	fs := a.newFlagSet("whoami")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	meta, err := c.Meta(context.Background())
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		_ = clikit.PrintJSON(a.out, map[string]any{
			"server": c.BaseURL,
			"meta":   meta,
			"config": a.cfgPath,
		})
		return 0
	}

	fmt.Fprintf(a.out, "服务端：%s\n", c.BaseURL)
	order := []string{"email", "role", "tenantId", "requireAuth", "identity", "provider", "agent", "policy", "version", "commit", "memory", "trace", "obs"}
	shown := map[string]bool{}
	for _, k := range order {
		if v, ok := meta[k]; ok && v != "" {
			fmt.Fprintf(a.out, "  %-12s %s\n", k+":", v)
			shown[k] = true
		}
	}
	// 其余字段也打出来（服务端以后加字段时，CLI 不必跟着改）。
	for k, v := range meta {
		if shown[k] || v == "" {
			continue
		}
		fmt.Fprintf(a.out, "  %-12s %s\n", k+":", v)
	}
	if a.cfgPath != "" {
		fmt.Fprintf(a.out, "配置：%s\n", a.cfgPath)
	}
	return 0
}
