package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
)

// 工作区文件浏览。
//
// 为什么控制面要读宿主文件系统：`local` provider 的 workspace 本来就在本机，
// 直接读是最短路径，还不用在沙箱里再起一个文件服务。代价是这只对 local/apple
// 成立——cube 的 workspace 在 microVM 里，得走 envd。所以能力声明成可选的
// sandbox.WorkspaceLocator，没实现就老老实实返回 501，而不是假装能读。
//
// 安全边界：所有路径都先按 root 相对化再 EvalSymlinks 复核。
// 只做 filepath.Join 是不够的——workspace 里一个指向 /etc 的软链就能绕过去。

const maxPreviewBytes = 512 << 10

var errOutsideRoot = errors.New("路径越出工作区")

type fileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

type fileContent struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// workspace 取会话对应沙箱的宿主工作区路径。
func (h *handler) workspace(sessionID string) (string, error) {
	if _, err := h.cfg.Store.GetSession(context.Background(), sessionID); err != nil {
		return "", err
	}
	loc, ok := h.cfg.Provider.(sandbox.WorkspaceLocator)
	if !ok {
		return "", sandbox.ErrUnsupported
	}
	return loc.HostWorkspace(sessionID)
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	// 文件面板能读到沙箱里的全部产物，归属校验不能省。
	if _, ok := h.getOwnedSession(w, r); !ok {
		return
	}
	root, err := h.workspace(r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	rel := r.URL.Query().Get("path")
	dir, err := resolveInRoot(root, rel)
	if err != nil {
		h.failErr(w, err)
		return
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		h.failErr(w, err)
		return
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue // 读不到的条目（权限/竞态删除）直接跳过，整个列表不该因一条而失败
		}
		out = append(out, fileEntry{
			Name:    e.Name(),
			Path:    joinRel(rel, e.Name()),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // 目录在前：找文件时不用翻过一堆产出物
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": cleanRel(rel), "entries": out})
}

func (h *handler) readFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedSession(w, r); !ok {
		return
	}
	root, err := h.workspace(r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	rel := r.URL.Query().Get("path")
	if cleanRel(rel) == "" {
		fail(w, http.StatusBadRequest, "path 不能为空")
		return
	}
	p, err := resolveInRoot(root, rel)
	if err != nil {
		h.failErr(w, err)
		return
	}

	f, err := os.Open(p)
	if err != nil {
		h.failErr(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		h.failErr(w, err)
		return
	}
	if st.IsDir() {
		fail(w, http.StatusBadRequest, "这是目录，用 /files 列内容")
		return
	}

	buf := make([]byte, maxPreviewBytes+1)
	n, _ := f.Read(buf)
	truncated := n > maxPreviewBytes
	if truncated {
		n = maxPreviewBytes
	}
	data := buf[:n]
	binary := looksBinary(data)
	out := fileContent{
		Path:      cleanRel(rel),
		Size:      st.Size(),
		Truncated: truncated,
		Binary:    binary,
	}
	if !binary {
		// 截断可能切在多字节字符中间，丢掉尾巴上不完整的那几个字节，
		// 否则前端会显示一个替换符，看起来像文件坏了。
		if truncated {
			for len(data) > 0 && !utf8.Valid(data) {
				data = data[:len(data)-1]
			}
		}
		out.Content = string(data)
	}
	writeJSON(w, http.StatusOK, out)
}

// downloadFile 原样吐出文件字节。
//
// 为什么不能复用 readFile：那个端点是给「预览」用的，它有三个对下载致命的约束——
// 二进制内容根本不返回（Binary=true 时 Content 为空）、超过 512KB 就截断、
// 响应体是 JSON（前端拿到的是转义后的字符串，写出来的文件必然损坏）。
// 手机要「把产出存下来/发出去」，就必须有一条返回原始字节的路径。
//
// 用 http.ServeContent 而不是自己 io.Copy：Content-Length、Range、If-Range、
// 416、按扩展名猜 Content-Type 这些它都处理过一遍，自己写只会漏边界（比如
// Range 越界时该回 416 而不是 200 全量）。这里只额外补下载必需的响应头。
func (h *handler) downloadFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.getOwnedSession(w, r); !ok {
		return
	}
	root, err := h.workspace(r.PathValue("id"))
	if err != nil {
		h.failErr(w, err)
		return
	}
	rel := r.URL.Query().Get("path")
	if cleanRel(rel) == "" {
		fail(w, http.StatusBadRequest, "path 不能为空")
		return
	}
	p, err := resolveInRoot(root, rel)
	if err != nil {
		h.failErr(w, err)
		return
	}

	f, err := os.Open(p)
	if err != nil {
		h.failErr(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		h.failErr(w, err)
		return
	}
	if st.IsDir() {
		fail(w, http.StatusBadRequest, "这是目录，不能下载")
		return
	}

	name := sanitizeFilename(path.Base(cleanRel(rel)))
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 不要缓存：workspace 是活的，同一个路径几秒后可能就是另一份内容。
	w.Header().Set("Cache-Control", "no-store")
	// ServeContent 会按文件名后缀定 Content-Type；后缀不认识时嗅探前 512 字节。
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// sanitizeFilename 把用户可控的文件名压成可以放进响应头的形状。
//
// 不能只做 URL 编码：Content-Disposition 是 HTTP 头，出现 CR/LF 就是头注入，
// 出现引号会截断 filename="..."。这里只留「名字」，把控制字符和引号去掉。
func sanitizeFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteRune('_')
		case r == '/' || r == '\r' || r == '\n' || r == 0:
			// 路径分隔符和控制字符都不该出现在文件名的位置上
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return "download"
	}
	return out
}

// contentDisposition 同时给 filename（ASCII 兜底）和 filename*（RFC 5987）。
// 只给 filename 的话中文名到手机上会变成一串问号；只给 filename* 的话
// 老客户端（部分安卓 WebView 下载器）会拿不到名字。
func contentDisposition(name string) string {
	ascii := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c >= 0x7f {
			ascii = append(ascii, '_')
			continue
		}
		ascii = append(ascii, c)
	}
	return `attachment; filename="` + string(ascii) + `"; filename*=UTF-8''` + rfc5987Escape(name)
}

// rfc5987Escape 只保留 unreserved 字符，其余一律百分号编码。
// 比 url.PathEscape 更严格是故意的：PathEscape 会把 `$&+,;=` 之类原样留下，
// 而这些字符在 Content-Disposition 参数里有歧义。
func rfc5987Escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// resolveInRoot 把用户给的相对路径钉死在 root 之内。
func resolveInRoot(root, rel string) (string, error) {
	root = filepath.Clean(root)
	// root 自己可能是软链（macOS 上 /var → /private/var），先取真实路径再比较。
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}

	rel = strings.TrimPrefix(strings.TrimSpace(rel), "/")
	p := root
	if rel != "" && rel != "." {
		p = filepath.Join(root, filepath.FromSlash(rel))
	}
	p = filepath.Clean(p)
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}

	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err // 不存在 → 上层映射成 404
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", errOutsideRoot
	}
	return real, nil
}

// joinRel 用 path 而不是 filepath：返回给前端的一律是 URL 风格的斜杠路径。
func joinRel(dir, name string) string {
	d := cleanRel(dir)
	if d == "" {
		return name
	}
	return path.Join(d, name)
}

func cleanRel(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

// looksBinary 用「前 8KB 里有没有 NUL」判断。
// 不做 MIME 猜测：猜错的代价是把二进制当文本渲染成乱码，比直接给个「无法预览」更糟。
func looksBinary(b []byte) bool {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return true
	}
	return false
}
