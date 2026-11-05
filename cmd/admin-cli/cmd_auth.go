package main

import (
	"context"
	"fmt"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

// cmdLogin 与 astra-cli 的 login 逻辑一致，只是配置落到 admin-cli.json，
// 并且多一个 --as 用于标记「这份配置是运营用的」。
func (a *app) cmdLogin(args []string) int {
	fs := a.newFlagSet("login")
	email := fs.String("email", "", "邮箱")
	password := fs.String("password", "", "密码")
	tenant := fs.String("tenant", "default", "租户 slug")
	as := fs.String("as", "admin", "写进配置的身份标记（不参与鉴权）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "email", "password", "tenant", "as", "json"))
	if done {
		return code
	}

	server := a.g.server
	if server == "" {
		server = a.cfg.Server
	}
	if server == "" {
		return a.fail(&client.UseError{Msg: "缺少 --server\n用法：admin-cli login --server http://127.0.0.1:7788 --token <token>"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	token := a.g.token
	if token == "" {
		token = a.cfg.Token
	}
	loginEmail := *email
	if token != "" && *email == "" && *password == "" {
		c := client.New(server, token)
		meta, err := c.Meta(ctx)
		if err != nil {
			return a.fail(fmt.Errorf("这个 token 用不了：%w", err))
		}
		loginEmail = meta["email"]
		if r := meta["role"]; r != "" && *as == "admin" {
			*as = r
		}
	} else {
		if *email == "" || *password == "" {
			return a.fail(&client.UseError{Msg: "需要 --token，或者 --email 与 --password 同时提供"})
		}
		c := client.New(server, "")
		res, err := c.Login(ctx, *tenant, *email, *password)
		if err != nil {
			return a.fail(err)
		}
		token = res.Token
		loginEmail = res.User.Email
		if res.User.Role != "" {
			*as = res.User.Role
		}
	}

	cfg := clikit.Config{Server: server, Token: token, Email: loginEmail, Tenant: *tenant, Role: *as}
	if a.cfgPath == "" {
		return a.fail(&client.UseError{Msg: "无法定位配置目录"})
	}
	if err := clikit.SaveConfig(a.cfgPath, cfg); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return a.jsonOut(cfg, nil)
	}
	fmt.Fprintf(a.out, "已登录 %s\n", server)
	if loginEmail != "" {
		fmt.Fprintf(a.out, "  账号：%s\n", loginEmail)
	}
	if *as != "" {
		fmt.Fprintf(a.out, "  身份：%s\n", *as)
	}
	fmt.Fprintf(a.out, "  凭据已保存到 %s（0600，与 astra-cli 分开）\n", a.cfgPath)
	// 运营命令要能看全租户；这里提前提示一下，省得跑到 403 才回头查角色。
	if *as != "" && *as != "owner" && *as != "admin" {
		fmt.Fprintf(a.err, "提示：当前身份 %q 可能没有管理权限，管理命令会返回 403\n", *as)
	}
	return 0
}

func (a *app) cmdWhoami(args []string) int {
	if wantsHelp(args) {
		fmt.Fprintln(a.out, "用法：admin-cli whoami")
		return 0
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	meta, err := c.Meta(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(map[string]any{"server": c.BaseURL, "meta": meta}, func() error {
		fmt.Fprintf(a.out, "服务端：%s\n", c.BaseURL)
		for _, k := range []string{"email", "role", "tenantId", "requireAuth"} {
			if v, ok := meta[k]; ok && v != "" {
				fmt.Fprintf(a.out, "  %-12s %s\n", k+":", v)
			}
		}
		fmt.Fprintf(a.out, "配置：%s\n", a.cfgPath)
		return nil
	})
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// microUSD 把整数微美元换成可读金额。库里用整数是为了避免浮点累加误差，
// 展示层只做一次除法。
func microUSD(v int64) string {
	return fmt.Sprintf("$%.4f", float64(v)/1e6)
}
