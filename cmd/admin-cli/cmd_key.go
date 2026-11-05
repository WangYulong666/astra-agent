package main

import (
	"context"
	"fmt"
	"time"

	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const keyHelp = `用法：
  admin-cli key list
  admin-cli key rotate [--name 名称] [--revoke-old] [--days 90]

说明：明文只在 rotate 的输出里出现一次；list 只有前缀。
`

func (a *app) cmdKey(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.keyList(args)
	case "rotate", "new":
		return a.keyRotate(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, keyHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "key 没有子命令 %q\n\n%s", sub, keyHelp)
		return 2
	}
}

func (a *app) keyList(args []string) int {
	fs := a.newFlagSet("key list")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	keys, err := c.AdminKeys(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(keys, func() error {
		t := clikit.Table{Header: []string{"PREFIX", "NAME", "STATE", "CREATED", "LAST USED", "EXPIRES", "ID"}}
		for _, k := range keys {
			state := "有效"
			if k.Revoked() {
				state = "已失效"
			}
			t.Rows = append(t.Rows, []string{
				k.Prefix, clikit.Trunc(k.Name, 20), state, formatTime(k.CreatedAt),
				formatTime(k.LastUsedAt), formatTime(k.ExpiresAt), k.ID,
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 把（明文不可再取，只能 rotate）\n", len(keys))
		return nil
	})
}

// keyRotate 签发新凭据，可选撤销同名旧凭据。
//
// 为什么默认不撤旧的：撤销是不可逆的，而「新钥匙能不能用」还得先验证。
// 让用户先拿到新明文、跑通一次，再 --revoke-old；如果反过来，
// 一旦新钥匙有问题，服务就被自己锁在门外了。
func (a *app) keyRotate(args []string) int {
	fs := a.newFlagSet("key rotate")
	name := fs.String("name", "", "凭据名称（默认 cli-<时间戳>）")
	revokeOld := fs.Bool("revoke-old", false, "签发成功后撤销同名旧凭据")
	days := fs.Int("days", 0, "有效期天数（0=不过期）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "name", "days"))
	if done {
		return code
	}
	keyName := *name
	if keyName == "" {
		keyName = "cli-" + time.Now().Format("20060102-150405")
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	res, err := c.AdminCreateKey(ctx, keyName, *days)
	if err != nil {
		return a.fail(err)
	}

	var revoked []string
	if *revokeOld {
		keys, err := c.AdminKeys(ctx)
		if err != nil {
			// 新钥匙已经签出来了，这里失败只报告，不回滚。
			fmt.Fprintf(a.err, "警告：新凭据已签发，但列旧凭据失败，未撤销任何旧凭据：%v\n", err)
		} else {
			for _, k := range keys {
				if k.ID == res.Key.ID || k.Name != keyName || k.Revoked() {
					continue
				}
				if err := c.AdminRevokeKey(ctx, k.ID); err != nil {
					fmt.Fprintf(a.err, "警告：撤销 %s 失败：%v\n", k.Prefix, err)
					continue
				}
				revoked = append(revoked, k.Prefix)
			}
		}
	}

	if a.g.json {
		return a.jsonOut(map[string]any{"key": res.Key, "plaintext": res.Plaintext, "revoked": revoked}, nil)
	}
	fmt.Fprintf(a.out, "已签发凭据 %s（%s）\n", res.Key.Name, res.Key.Prefix)
	fmt.Fprintf(a.out, "明文（只显示一次，请立刻保存）：\n\n  %s\n\n", res.Plaintext)
	if len(revoked) > 0 {
		fmt.Fprintf(a.out, "已撤销同名旧凭据：%v\n", revoked)
	}
	fmt.Fprintf(a.out, "用法：astra-cli login --server %s --token <上面的明文>\n", c.BaseURL)
	return 0
}
