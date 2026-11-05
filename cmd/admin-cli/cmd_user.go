package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const userHelp = `用法：
  admin-cli user list [--tenant ID]
  admin-cli user create --email E [--name 显示名] [--role member|admin|owner] [--password P]
  admin-cli user reset-password <用户ID> [--password 新密码] [--yes]
`

func (a *app) cmdUser(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.userList(args)
	case "create":
		return a.userCreate(args)
	case "reset-password", "passwd":
		return a.userResetPassword(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, userHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "user 没有子命令 %q\n\n%s", sub, userHelp)
		return 2
	}
}

func (a *app) userList(args []string) int {
	fs := a.newFlagSet("user list")
	tenant := fs.String("tenant", "", "租户 ID（仅用于校验；服务端按当前身份过滤）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "tenant"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	// 服务端只按 principal 的租户过滤，没有「列别的租户」的参数。
	// 传了不匹配的 --tenant 就在本地先拦住，否则会拿到「看似别人的」列表。
	if *tenant != "" {
		meta, err := c.Meta(ctx)
		if err == nil && meta["tenantId"] != "" && meta["tenantId"] != *tenant {
			return a.fail(&client.UseError{Msg: fmt.Sprintf(
				"当前身份属于租户 %s，不能列出租户 %s 的用户（服务端不支持跨租户查询，这是安全设计）",
				meta["tenantId"], *tenant)})
		}
	}
	users, err := c.AdminUsers(ctx)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(users, func() error {
		t := clikit.Table{Header: []string{"EMAIL", "NAME", "ROLE", "STATUS", "LAST LOGIN", "ID"}}
		for _, u := range users {
			t.Rows = append(t.Rows, []string{
				u.Email, clikit.Trunc(u.DisplayName, 16), u.Role, u.Status, formatTime(u.LastLoginAt), u.ID,
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 个用户\n", len(users))
		return nil
	})
}

func (a *app) userCreate(args []string) int {
	fs := a.newFlagSet("user create")
	email := fs.String("email", "", "邮箱")
	name := fs.String("name", "", "显示名")
	role := fs.String("role", "", "角色：owner | admin | member（默认 member）")
	password := fs.String("password", "", "初始密码（留空则由服务端生成并只显示一次）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "email", "name", "role", "password"))
	if done {
		return code
	}
	if *email == "" {
		return a.fail(&client.UseError{Msg: "缺少 --email\n用法：admin-cli user create --email E [--role member]"})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	res, err := c.AdminCreateUser(context.Background(), *email, *name, *role, *password)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(res, func() error {
		fmt.Fprintf(a.out, "已创建用户 %s（角色 %s，ID %s）\n", res.User.Email, res.User.Role, res.User.ID)
		if res.InitialPassword != "" {
			// 明文只出现这一次，服务端不留、也再取不回。
			fmt.Fprintf(a.out, "初始密码（只显示一次）：%s\n", res.InitialPassword)
		}
		return nil
	})
}

func (a *app) userResetPassword(args []string) int {
	fs := a.newFlagSet("user reset-password")
	password := fs.String("password", "", "新密码（留空则随机生成）")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "password"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli user reset-password <用户ID> [--password 新密码] [--yes]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	// 重置密码会让该用户所有 token 立即失效（服务端推进 TokenValidAfter），
	// 等于把人踢下线——属于「影响别人」的操作，默认必须确认。
	if !a.confirm(fmt.Sprintf("重置用户 %s 的密码会让其所有登录态立即失效，继续？", pos[0])) {
		fmt.Fprintln(a.out, "已取消")
		return 1
	}
	pwd := *password
	generated := false
	if pwd == "" {
		pwd, err = randomPassword(16)
		if err != nil {
			return a.fail(err)
		}
		generated = true
	}
	u, err := c.AdminResetPassword(context.Background(), pos[0], pwd)
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		out := map[string]any{"user": u}
		if generated {
			out["password"] = pwd
		}
		return a.jsonOut(out, nil)
	}
	fmt.Fprintf(a.out, "已重置 %s 的密码\n", u.Email)
	if generated {
		fmt.Fprintf(a.out, "新密码（只显示一次）：%s\n", pwd)
	}
	return 0
}

// randomPassword 生成不含易混字符（0/O/1/l/I）的密码，
// 因为运维会把它念给人听或贴进聊天窗口。
func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}
