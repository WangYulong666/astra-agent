package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const tenantHelp = `用法：
  admin-cli tenant list
  admin-cli tenant create --name 名称 --slug 短名 [--limit 1000000]
  admin-cli tenant suspend <租户ID> [--yes]
  admin-cli tenant resume <租户ID>
`

func (a *app) cmdTenant(sub string, args []string) int {
	switch sub {
	case "list", "ls":
		return a.tenantList(args)
	case "create":
		return a.tenantCreate(args)
	case "suspend":
		return a.tenantSuspend(args)
	case "resume", "activate":
		return a.tenantResume(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, tenantHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "tenant 没有子命令 %q\n\n%s", sub, tenantHelp)
		return 2
	}
}

// tenantList 带用量和状态。
//
// 用量的来源是 /v1/admin/usage：它按调用者身份过滤，所以普通租户只会看到
// 自己那一行有数字，其它租户显示 "-"。这是服务端的安全设计（不能横向看别人
// 的账），不是 CLI 取不到——超管（兼容模式）运行时能看到全部。
func (a *app) tenantList(args []string) int {
	fs := a.newFlagSet("tenant list")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	tenants, err := c.AdminTenants(ctx)
	if err != nil {
		return a.fail(err)
	}
	usage, uerr := c.AdminUsage(ctx, client.UsageQuery{Limit: 5000})
	// 用量拿不到不该让租户列表整个失败：治理接口可能是只读账号，
	// 但「有哪些租户、谁是 suspended」仍然必须能看到。
	if uerr != nil && a.g.verbose {
		fmt.Fprintf(a.err, "[v] 取用量失败（列表继续）：%v\n", uerr)
	}

	type agg struct {
		tokens int64
		cost   int64
		calls  int64
	}
	byTenant := map[string]*agg{}
	for _, it := range usage.Items {
		g := byTenant[it.TenantID]
		if g == nil {
			g = &agg{}
			byTenant[it.TenantID] = g
		}
		g.tokens += it.TotalTokens()
		g.cost += it.CostMicroUSD
		g.calls++
	}

	return a.jsonOut(map[string]any{"tenants": tenants, "usage": usage}, func() error {
		t := clikit.Table{
			Header: []string{"ID", "SLUG", "NAME", "STATUS", "LIMIT", "USED(TOKENS)", "COST", "CREATED"},
			Right:  map[int]bool{4: true, 5: true, 6: true},
		}
		for _, tn := range tenants {
			limit := "不限"
			if tn.MonthlyTokenLimit > 0 {
				limit = fmt.Sprintf("%d", tn.MonthlyTokenLimit)
			}
			used, cost := "-", "-"
			if g, ok := byTenant[tn.ID]; ok && g.calls > 0 {
				used = fmt.Sprintf("%d", g.tokens)
				cost = microUSD(g.cost)
			}
			if uerr != nil && tn.ID != "" {
				// 没有用量权限时不要假装是 0。
				used, cost = "n/a", "n/a"
			}
			t.Rows = append(t.Rows, []string{
				tn.ID, tn.Slug, clikit.Trunc(tn.Name, 18), tn.Status, limit, used, cost, formatTime(tn.CreatedAt),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 个租户", len(tenants))
		if uerr != nil {
			fmt.Fprintf(a.out, "（用量不可见：%v）", uerr)
		}
		fmt.Fprintln(a.out)
		return nil
	})
}

func (a *app) tenantCreate(args []string) int {
	fs := a.newFlagSet("tenant create")
	name := fs.String("name", "", "租户名称")
	slug := fs.String("slug", "", "租户短名（登录用，小写字母数字连字符）")
	limit := fs.Int64("limit", 0, "月度 token 上限（0=不限）")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "name", "slug", "limit"))
	if done {
		return code
	}
	_ = pos
	if *name == "" || *slug == "" {
		return a.fail(&client.UseError{Msg: "--name 与 --slug 都必填\n用法：admin-cli tenant create --name 名称 --slug 短名 [--limit N]"})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	tn, err := c.AdminCreateTenant(context.Background(), *name, *slug, *limit)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(tn, func() error {
		fmt.Fprintf(a.out, "已创建租户 %s（slug=%s）\n", tn.ID, tn.Slug)
		return nil
	})
}

func (a *app) tenantSuspend(args []string) int {
	fs := a.newFlagSet("tenant suspend")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli tenant suspend <租户ID> [--yes]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	tn, err := c.AdminTenant(ctx, pos[0])
	if err != nil {
		return a.fail(err)
	}
	// 停用会让该租户所有请求 403，等于全公司停服——必须说清楚影响再问。
	if !a.confirm(fmt.Sprintf("停用租户 %s（%s）后，该租户所有用户将立即无法登录/调用，继续？", tn.Slug, tn.Name)) {
		fmt.Fprintln(a.out, "已取消")
		return 1
	}
	tn, err = c.AdminSetTenantStatus(ctx, pos[0], "suspended")
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(tn, func() error {
		fmt.Fprintf(a.out, "已停用租户 %s（状态 %s）\n", tn.Slug, tn.Status)
		return nil
	})
}

func (a *app) tenantResume(args []string) int {
	fs := a.newFlagSet("tenant resume")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "admin-cli tenant resume <租户ID>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	tn, err := c.AdminSetTenantStatus(context.Background(), pos[0], "active")
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(tn, func() error {
		fmt.Fprintf(a.out, "已恢复租户 %s（状态 %s）\n", tn.Slug, tn.Status)
		return nil
	})
}
