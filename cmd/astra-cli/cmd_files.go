package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/WangYulong666/astra-agent/internal/client/clikit"
)

const filesHelp = `用法：
  astra-cli files ls <会话ID> [路径]              列出工作区文件
  astra-cli files cat <会话ID> <路径>             直接输出文件内容（预览，512KB 截断）
  astra-cli files download <会话ID> <路径> [--out 本地路径]
`

// cmdFiles 实现 files ls|cat|download。
//
// 三个动作对应三个端点，不能互相替代：
//   - ls       → GET /files（目录列表）
//   - cat      → GET /file（JSON 预览，512KB 截断、二进制不返内容）
//   - download → GET /download（原始字节，落盘必须用它）
func (a *app) cmdFiles(sub string, args []string) int {
	switch sub {
	case "ls", "list":
		return a.filesLs(args)
	case "cat", "read":
		return a.filesCat(args)
	case "download", "dl":
		return a.filesDownload(args)
	default:
		if wantsHelp(args) || sub == "" {
			fmt.Fprint(a.out, filesHelp)
			if sub == "" && !wantsHelp(args) {
				return 2
			}
			return 0
		}
		fmt.Fprintf(a.err, "files 没有子命令 %q\n\n%s", sub, filesHelp)
		return 2
	}
}

func (a *app) filesLs(args []string) int {
	fs := a.newFlagSet("files ls")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 1, "astra-cli files ls <会话ID> [路径]"); err != nil {
		return a.fail(err)
	}
	path := ""
	if len(pos) > 1 {
		path = pos[1]
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	list, err := c.ListFiles(context.Background(), pos[0], path)
	if err != nil {
		return a.fail(err)
	}
	return a.jsonOut(list, func() error {
		t := clikit.Table{
			Header: []string{"TYPE", "SIZE", "MODIFIED", "NAME"},
			Right:  map[int]bool{1: true},
		}
		for _, e := range list.Entries {
			kind := "file"
			size := clikit.HumanSize(e.Size)
			if e.IsDir {
				kind, size = "dir", "-"
			}
			t.Rows = append(t.Rows, []string{kind, size, formatTime(e.ModTime), e.Name})
		}
		t.Render(a.out)
		fmt.Fprintf(a.out, "\n%s · %d 项\n", displayPath(list.Path), len(list.Entries))
		return nil
	})
}

func displayPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func (a *app) filesCat(args []string) int {
	fs := a.newFlagSet("files cat")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 2, "astra-cli files cat <会话ID> <路径>"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}
	f, err := c.ReadFile(context.Background(), pos[0], pos[1])
	if err != nil {
		return a.fail(err)
	}
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, f))
	}
	if f.Binary {
		// 二进制不往终端糊：服务端本来就不返回内容，这里只是把原因说清楚。
		fmt.Fprintf(a.err, "%s 是二进制文件（%s），用 files download 落盘\n", f.Path, clikit.HumanSize(f.Size))
		return 0
	}
	fmt.Fprint(a.out, f.Content)
	if f.Truncated {
		fmt.Fprintf(a.err, "\n[已截断：只显示前 %s，完整内容用 files download]\n", clikit.HumanSize(int64(len(f.Content))))
	}
	return 0
}

func (a *app) filesDownload(args []string) int {
	fs := a.newFlagSet("files download")
	out := fs.String("out", "", "本地保存路径（默认当前目录 + 原文件名）")
	pos, code, done := a.parse(fs, args, clikit.BoolFlags("server", "token", "out"))
	if done {
		return code
	}
	if err := a.needArgs(pos, 2, "astra-cli files download <会话ID> <路径> [--out 本地路径]"); err != nil {
		return a.fail(err)
	}
	c, err := a.api()
	if err != nil {
		return a.fail(err)
	}

	dst := *out
	if dst == "" {
		dst = filepath.Base(pos[1])
	}
	// 先写临时文件再改名：下载中途断了不会留下一个「看起来下完了」的半个文件。
	tmp := dst + ".part"
	fh, err := os.Create(tmp)
	if err != nil {
		return a.fail(fmt.Errorf("创建 %s 失败: %w", tmp, err))
	}
	n, err := c.DownloadFile(context.Background(), pos[0], pos[1], fh)
	closeErr := fh.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return a.fail(err)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return a.fail(closeErr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return a.fail(err)
	}
	abs, _ := filepath.Abs(dst)
	if a.g.json {
		return intOrZero(clikit.PrintJSON(a.out, map[string]any{
			"sessionId": pos[0], "path": pos[1], "savedTo": abs, "bytes": n,
		}))
	}
	fmt.Fprintf(a.out, "已保存 %s（%s）\n", abs, clikit.HumanSize(n))
	return 0
}
