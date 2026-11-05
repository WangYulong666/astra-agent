package api

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// 前端构建产物直接嵌进二进制。
//
// 取舍：提交 dist（一份 ~200KB 的产物）换来「go build 不需要 node」。
// 反过来做（运行时读磁盘）就意味着发布要带两个产物，而控制面又退化成
// 「启动目录错一层就白屏」。源码在 web/，`make web` 重新生成 dist。
//
//go:embed all:web/dist
var distFS embed.FS

// staticHandler 提供前端静态资源，并做 SPA 兜底。
//
// 兜底只对「不像资源请求」的路径生效：带扩展名却找不到的返回 404，
// 否则一个拼错的 /assets/x.js 会拿到 200 + HTML，浏览器报 MIME 错误，
// 排查时看起来像构建坏了。
func staticHandler() http.Handler {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		// embed 的路径在编译期就固定了，走到这里说明代码和目录结构不一致，
		// 属于开发期错误，panic 比运行时白屏更容易发现。
		panic("api: 前端产物缺失（先跑 make web）: " + err.Error())
	}

	// 不用 http.FileServer：它会把 /index.html 301 到 ./，
	// 而我们把 / 映射成 index.html，于是形成 / → /index.html → / 的重定向环
	// （浏览器直接报 ERR_TOO_MANY_REDIRECTS）。自己读文件 + ServeContent 更可控，
	// 顺带拿到 If-Modified-Since 与 Range 支持。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}

		if st, err := fs.Stat(sub, p); err != nil || st.IsDir() {
			if strings.Contains(p, ".") {
				http.NotFound(w, r)
				return
			}
			p = "index.html" // 前端路由（/s/:id 之类）交给 SPA
		}

		st, err := fs.Stat(sub, p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if strings.HasPrefix(p, "assets/") {
			// 文件名带内容哈希，可以永久缓存。
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		http.ServeContent(w, r, p, st.ModTime(), bytes.NewReader(data))
	})
}
