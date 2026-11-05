package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client"
	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const memoryHelp = `用法：
  astra-cli memory search "关键词" [--k 10] [--kind fact|conversation]
  astra-cli memory add "内容" [--kind fact] [--session ID]
  astra-cli memory stats
  astra-cli memory rm <记忆ID> [--yes]

说明：检索与删除永远只作用于当前账号，没有指定别人的参数。
`

// cmdMemory 实现 memory search|add|stats。
//
// 这里刻意没有 --user 之类的参数：服务端按「当前身份」过滤，
// 客户端根本拿不到别人的记忆。加这样的参数只会让用户以为可以。
func (a *app) cmdMemory(sub string, args []string) int {
	switch sub {
	case "search", "find":
		return a.memorySearch(args)
	case "add", "ingest":
		return a.memoryAdd(args)
	case "stats":
		return a.memoryStats(args)
	case "rm", "delete":
		return a.memoryDelete(args)
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

func (a *app) memorySearch(args []string) int {
	fs := a.newFlagSet("memory search")
	k := fs.Int("k", 10, "返回条数（上限 50）")
	kind := fs.String("kind", "", "只搜某类：fact | conversation")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "k", "kind"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli memory search \"关键词\" [--k 10] [--kind fact]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	items, err := c.MemorySearch(context.Background(), pos[0], *k, *kind)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(items, func() error {
		t := clikit.Table{
			Header: []string{"SCORE", "KIND", "CREATED", "TEXT"},
			Right:  map[int]bool{0: true},
		}
		for _, it := range items {
			t.Rows = append(t.Rows, []string{
				fmt.Sprintf("%.3f", it.Score),
				it.Kind,
				formatTime(it.CreatedAt),
				clikit.Trunc(it.Text, 70),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n命中 %d 条\n", len(items))
		return nil
	})
}

func (a *app) memoryAdd(args []string) int {
	fs := a.newFlagSet("memory add")
	kind := fs.String("kind", "fact", "记忆类型：fact | conversation")
	session := fs.String("session", "", "关联会话 ID")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "kind", "session"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli memory add \"内容\" [--kind fact] [--session ID]"); err != nil {
		return a.fail(err)
	}
	if *kind != "fact" && *kind != "conversation" {
		return a.fail(&client.UseError{Msg: "--kind 只能是 fact 或 conversation"})
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	it, err := c.MemoryIngest(context.Background(), pos[0], *kind, *session, nil)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(it, func() error {
		fmt.Fprintf(a.out, "已写入记忆 %s（%s）\n", it.ID, it.Kind)
		return nil
	})
}

func (a *app) memoryStats(args []string) int {
	fs := a.newFlagSet("memory stats")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
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
		state := "已启用"
		if !st.Enabled {
			state = "未启用（服务端未配置 MILVUS_ADDR）"
		}
		fmt.Fprintf(a.out, "记忆：%s\n", state)
		if st.Enabled {
			fmt.Fprintf(a.out, "  后端：%s\n", dash(st.Backend))
			fmt.Fprintf(a.out, "  集合：%s\n", dash(st.Collection))
			fmt.Fprintf(a.out, "  条数：%d\n", st.Count)
			fmt.Fprintf(a.out, "  维度：%d\n", st.Dim)
			fmt.Fprintf(a.out, "  Embedder：%s\n", dash(st.Embedder))
		}
		return nil
	})
}

func (a *app) memoryDelete(args []string) int {
	fs := a.newFlagSet("memory rm")
	yes := fs.Bool("yes", false, "跳过确认")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli memory rm <记忆ID> [--yes]"); err != nil {
		return a.fail(err)
	}
	if !*yes && !a.g.json {
		if !clikit.Confirm(a.in(), a.out, "删除记忆不可恢复，继续？") {
			fmt.Fprintln(a.out, "已取消")
			return 1
		}
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	if err := c.MemoryDelete(context.Background(), pos[0]); err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]string{"deleted": pos[0]}))
	}
	fmt.Fprintf(a.out, "已删除记忆 %s\n", pos[0])
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
