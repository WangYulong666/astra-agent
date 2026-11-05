package main

import (
	"context"
	"fmt"

	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const obsHelp = `用法：
  admin-cli obs status
  admin-cli obs traces [--run RUN_ID] [--limit 20]
`

func (a *app) cmdObs(sub string, args []string) int {
	switch sub {
	case "status":
		return a.obsStatus(args)
	case "traces", "trace":
		return a.obsTraces(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, obsHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "obs 没有子命令 %q\n\n%s", sub, obsHelp)
		return 2
	}
}

// obsStatus 回答「观测开没开、上报到哪、没开该配哪几个变量」。
//
// 这是运维最常问的一句话，所以未启用时不能只说「未启用」——
// 必须把要配的变量名列出来，否则每次都要去翻文档。
func (a *app) obsStatus(args []string) int {
	fs := a.newFlagSet("obs status")
	if _, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token")); done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	cfg, err := c.ObsConfig(context.Background())
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(cfg, func() error {
		if cfg.Enabled {
			fmt.Fprintln(a.out, "观测：已启用")
			fmt.Fprintf(a.out, "  endpoint：  %s\n", dash(cfg.Endpoint))
			fmt.Fprintf(a.out, "  dashboard： %s\n", dash(cfg.DashboardURL))
			fmt.Fprintf(a.out, "  public key：%s\n", maskKey(cfg.PublicKey))
			fmt.Fprintln(a.out, "  secret key：服务端持有，不下发")
			return nil
		}
		fmt.Fprintln(a.out, "观测：未启用")
		fmt.Fprintln(a.out, "")
		fmt.Fprintln(a.out, "启用需要在服务端配置以下环境变量后重启：")
		fmt.Fprintln(a.out, "  LANGFUSE_HOST          Langfuse 地址，如 http://127.0.0.1:3000")
		fmt.Fprintln(a.out, "  LANGFUSE_PUBLIC_KEY    Langfuse 项目公钥")
		fmt.Fprintln(a.out, "  LANGFUSE_SECRET_KEY    Langfuse 项目密钥（只在服务端）")
		fmt.Fprintln(a.out, "  LANGFUSE_DASHBOARD_URL 可选，dashboard 跳转地址，默认同 LANGFUSE_HOST")
		fmt.Fprintln(a.out, "  ASTRA_AGENT_NAME       可选，trace 里的 agent 标签，默认 astra")
		fmt.Fprintln(a.out, "")
		fmt.Fprintln(a.out, "本地起 Langfuse：make obs-up（见 deploy/observability）")
		return nil
	})
}

func maskKey(k string) string {
	if len(k) <= 8 {
		if k == "" {
			return "-"
		}
		return k
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func (a *app) obsTraces(args []string) int {
	fs := a.newFlagSet("obs traces")
	run := fs.String("run", "", "只看某个 run")
	limit := fs.Int("limit", 20, "返回条数")
	_, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "run", "limit"))
	if done {
		return code
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	traces, err := c.ObsTraces(context.Background(), *run, *limit)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(traces, func() error {
		t := clikit.Table{
			Header: []string{"STARTED", "NAME", "RUN", "STATUS", "LATENCY", "URL"},
			Right:  map[int]bool{4: true},
		}
		for _, tr := range traces {
			lat := "-"
			if tr.LatencyMS > 0 {
				lat = fmt.Sprintf("%dms", tr.LatencyMS)
			}
			t.Rows = append(t.Rows, []string{
				formatTime(tr.StartedAt), clikit.Trunc(tr.Name, 24), dash(tr.RunID), dash(tr.Status),
				lat, clikit.Trunc(tr.URL, 40),
			})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n共 %d 条 trace\n", len(traces))
		return nil
	})
}
