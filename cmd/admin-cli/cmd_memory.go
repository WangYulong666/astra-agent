package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const memoryHelp = `用法：
  admin-cli memory stats [--tenant ID]

说明：只读统计（集合、条数、维度），**不返回任何记忆内容**。
服务端 /v1/memory/stats 是集合级统计，没有按租户拆分的接口——
所以 --tenant 只用于确认「你要看的是哪个租户」，并会明确提示统计口径。
`

func (a *app) cmdMemory(sub string, args []string) int {
	switch sub {
	case "stats", "stat":
		return a.memoryStats(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, memoryHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "memory 没有子命令 %q\n\n%s", sub, memoryHelp)
		return 2
	}
}

func (a *app) memoryStats(args []string) int {
	fs := a.newFlagSet("memory stats")
	tenant := fs.String("tenant", "", "租户 ID（仅用于核对口径）")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "tenant"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	st, err := c.MemoryStats(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(st, func() error {
		if !st.Enabled {
			fmt.Fprintln(a.out, "记忆：未启用")
			fmt.Fprintln(a.out, "")
			fmt.Fprintln(a.out, "启用需要在服务端配置：")
			fmt.Fprintln(a.out, "  MILVUS_ADDR          Milvus 地址，如 127.0.0.1:19530")
			fmt.Fprintln(a.out, "  MILVUS_COLLECTION    可选，默认 astra_memory")
			fmt.Fprintln(a.out, "  EMBEDDING_*          Embedder 配置（无 key 时退化为 hash，仅开发用）")
			fmt.Fprintln(a.out, "")
			fmt.Fprintln(a.out, "本地起 Milvus：make vector-up（见 deploy/vector）")
			return nil
		}
		fmt.Fprintln(a.out, "记忆：已启用")
		fmt.Fprintf(a.out, "  后端：     %s\n", dash(st.Backend))
		fmt.Fprintf(a.out, "  集合：     %s\n", dash(st.Collection))
		fmt.Fprintf(a.out, "  条数：     %d\n", st.Count)
		fmt.Fprintf(a.out, "  向量维度： %d\n", st.Dim)
		fmt.Fprintf(a.out, "  Embedder： %s\n", dash(st.Embedder))
		if *tenant != "" {
			fmt.Fprintf(a.out, "\n注意：以上是集合级统计（%s），服务端没有按租户拆分的接口；\n", *tenant)
			fmt.Fprintln(a.out, "      单租户条数只能通过 astra-cli memory stats（用户视角）间接确认。")
		}
		return nil
	})
}
