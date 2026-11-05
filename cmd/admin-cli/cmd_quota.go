package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const quotaHelp = `用法：
  admin-cli quota show [--tenant ID]
  admin-cli quota set --tenant ID --limit 1000000   （0 表示不限）

说明：用量只能看当前身份所属租户（服务端按身份过滤，不支持跨租户）。
`

func (a *app) cmdQuota(sub string, args []string) int {
	switch sub {
	case "show", "get":
		return a.quotaShow(args)
	case "set":
		return a.quotaSet(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, quotaHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "quota 没有子命令 %q\n\n%s", sub, quotaHelp)
		return 2
	}
}

// quotaShow 展示「上限 / 已用 / 剩余」。
//
// 剩余额度是本地算的：服务端只存上限、只报用量，没有「剩余」这个概念
// （额度是软限制，超限后拒绝新 run，但已有 run 照样跑完）。
func (a *app) quotaShow(args []string) int {
	fs := a.newFlagSet("quota show")
	tenant := fs.String("tenant", "", "租户 ID（默认当前身份所属租户）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "tenant"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	meta, err := c.Meta(ctx)
	if err != nil {
		return a.fail(err)
	}
	tenantID := *tenant
	if tenantID == "" {
		tenantID = meta["tenantId"]
	}
	if tenantID == "" {
		return a.fail(&client.UseError{Msg: "拿不到租户 ID：请用 --tenant 指定"})
	}
	if own := meta["tenantId"]; own != "" && tenantID != own {
		return a.fail(&client.UseError{Msg: fmt.Sprintf(
			"不能查租户 %s 的用量：服务端只返回当前身份所属租户（%s）的数据", tenantID, own)})
	}
	tn, err := c.AdminTenant(ctx, tenantID)
	if err != nil {
		return a.fail(err)
	}
	usage, err := c.AdminUsage(ctx, client.UsageQuery{Limit: 5000})
	if err != nil {
		return a.fail(err)
	}

	used := usage.Sum.TotalTokens()
	var remaining string
	if tn.MonthlyTokenLimit <= 0 {
		remaining = "不限"
	} else if r := tn.MonthlyTokenLimit - used; r > 0 {
		remaining = fmt.Sprintf("%d", r)
	} else {
		remaining = fmt.Sprintf("已超 %d", -r)
	}

	result := map[string]any{
		"tenant":    tn,
		"used":      usage.Sum,
		"remaining": remaining,
	}
	return a.jsonOut(result, func() error {
		fmt.Fprintf(a.out, "租户：%s（%s）\n", tn.Slug, tn.Name)
		limit := "不限"
		if tn.MonthlyTokenLimit > 0 {
			limit = fmt.Sprintf("%d", tn.MonthlyTokenLimit)
		}
		fmt.Fprintf(a.out, "  月度上限：%s tokens\n", limit)
		fmt.Fprintf(a.out, "  已用：    %d tokens（入 %d / 出 %d，%d 次调用）\n",
			used, usage.Sum.InputTokens, usage.Sum.OutputTokens, usage.Sum.Calls)
		fmt.Fprintf(a.out, "  剩余：    %s\n", remaining)
		fmt.Fprintf(a.out, "  花费：    %s\n", microUSD(usage.Sum.CostMicroUSD))
		return nil
	})
}

func (a *app) quotaSet(args []string) int {
	fs := a.newFlagSet("quota set")
	tenant := fs.String("tenant", "", "租户 ID")
	limit := fs.Int64("limit", -1, "月度 token 上限（0 表示不限）")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "tenant", "limit"))
	if done {
		return code
	}
	// 允许 `quota set --tenant X 1000000` 这种写法：位置参数当 limit。
	if *limit < 0 && len(pos) > 0 {
		var n int64
		if _, err := fmt.Sscanf(pos[0], "%d", &n); err == nil {
			*limit = n
		}
	}
	if *limit < 0 {
		return a.fail(&client.UseError{Msg: "缺少 --limit\n用法：admin-cli quota set --tenant ID --limit 1000000"})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	ctx := context.Background()
	tenantID := *tenant
	if tenantID == "" {
		meta, err := c.Meta(ctx)
		if err != nil {
			return a.fail(err)
		}
		tenantID = meta["tenantId"]
	}
	if tenantID == "" {
		return a.fail(&client.UseError{Msg: "缺少 --tenant"})
	}
	tn, err := c.AdminSetTenantQuota(ctx, tenantID, *limit)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(tn, func() error {
		if tn.MonthlyTokenLimit == 0 {
			fmt.Fprintf(a.out, "已将租户 %s 的月度上限设为不限\n", tn.Slug)
		} else {
			fmt.Fprintf(a.out, "已将租户 %s 的月度上限设为 %d tokens\n", tn.Slug, tn.MonthlyTokenLimit)
		}
		return nil
	})
}
