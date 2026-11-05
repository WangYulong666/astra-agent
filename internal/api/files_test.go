package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WangYulong666/astra-agent/internal/sandbox"
)

// 这组测试只盯下载端点。动机是预览端点和下载端点的契约完全不同：
// 预览会把二进制藏起来、把大文件截断、把字节塞进 JSON 字符串，
// 而下载必须一个字节不差——所以它值得有自己的一组断言。

func (h *harness) workspaceOf(t *testing.T, sessionID string) string {
	t.Helper()
	loc, ok := h.p.(sandbox.WorkspaceLocator)
	if !ok {
		t.Fatal("local provider 没有实现 WorkspaceLocator")
	}
	ws, err := loc.HostWorkspace(sessionID)
	if err != nil {
		t.Fatalf("取 workspace: %v", err)
	}
	return ws
}

func (h *harness) download(t *testing.T, sessionID, rel string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		h.srv.URL+"/v1/sessions/"+sessionID+"/download?path="+rel, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 二进制内容必须原样返回：这正是 /file 做不到的那件事（Binary=true 时 Content 为空）。
func TestDownloadKeepsBinaryBytes(t *testing.T) {
	h := newHarness(t)
	s := h.newSession(t)
	ws := h.workspaceOf(t, s.SandboxID)

	// 512KB 是预览上限，故意跨过它，并把 0x00 塞进去（会让预览判成二进制）
	raw := make([]byte, maxPreviewBytes+777)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	raw[10] = 0
	raw[len(raw)-1] = 0xFF
	if err := os.WriteFile(filepath.Join(ws, "out.bin"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.download(t, s.ID, "out.bin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Length"), jsonNumber(len(raw)); got != want {
		t.Errorf("Content-Length = %q，期望 %q", got, want)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, `attachment; filename="out.bin"`) {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, raw) {
		t.Fatalf("下载内容与源文件不一致：%d 字节 vs %d 字节", len(body), len(raw))
	}
}

// 中文名必须走 filename*，否则到手机上是一串问号；
// 同时 ASCII 兜底里不能残留原始 UTF-8 字节（老客户端会把它当 GBK 解）。
func TestDownloadChineseFilenameHeader(t *testing.T) {
	h := newHarness(t)
	s := h.newSession(t)
	ws := h.workspaceOf(t, s.SandboxID)
	if err := os.WriteFile(filepath.Join(ws, "报告.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.download(t, s.ID, "%E6%8A%A5%E5%91%8A.md", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "filename*=UTF-8''%E6%8A%A5%E5%91%8A.md") {
		t.Errorf("Content-Disposition 缺少 filename*：%q", cd)
	}
	for _, r := range cd {
		if r > 0x7f {
			t.Fatalf("响应头里出现了非 ASCII 字符：%q", cd)
		}
	}
}

// Range 支持是「大文件以后能续传」的前提，而它完全由 ServeContent 提供——
// 所以这里测的是「我们没有在它前面把响应写脏」，不是测标准库。
func TestDownloadRange(t *testing.T) {
	h := newHarness(t)
	s := h.newSession(t)
	ws := h.workspaceOf(t, s.SandboxID)
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := h.download(t, s.ID, "a.txt", map[string]string{"Range": "bytes=2-5"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("状态码 = %d，期望 206", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "2345" {
		t.Errorf("Range 内容 = %q，期望 2345", string(body))
	}

	bad := h.download(t, s.ID, "a.txt", map[string]string{"Range": "bytes=99-200"})
	if bad.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("越界 Range 状态码 = %d，期望 416", bad.StatusCode)
	}
}

func TestDownloadRejects(t *testing.T) {
	h := newHarness(t)
	s := h.newSession(t)
	ws := h.workspaceOf(t, s.SandboxID)
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"空路径", "", http.StatusBadRequest},
		{"目录", "sub", http.StatusBadRequest},
		{"不存在", "nope.txt", http.StatusNotFound},
		{"路径穿越", "../../../../etc/passwd", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := h.download(t, s.ID, c.path, nil).StatusCode; got != c.want {
				t.Fatalf("状态码 = %d，期望 %d", got, c.want)
			}
		})
	}
}

// 文件名是用户可控的（agent 可以随手写一个带换行的名字），
// 直接拼进 Content-Disposition 就是响应头注入。
func TestSanitizeFilenameStripsHeaderBreak(t *testing.T) {
	for _, in := range []string{"a\r\nX-Evil: 1.txt", "a\"b.txt", "a\\b.txt", "a\x00b"} {
		out := sanitizeFilename(in)
		if strings.ContainsAny(out, "\r\n\"\\\x00") {
			t.Errorf("sanitizeFilename(%q) = %q，仍含危险字符", in, out)
		}
	}
	for _, in := range []string{"", ".", "..", "   "} {
		if got := sanitizeFilename(in); got != "download" {
			t.Errorf("sanitizeFilename(%q) = %q，期望 download", in, got)
		}
	}
	if got := sanitizeFilename("报告 1.png"); got != "报告 1.png" {
		t.Errorf("中文名被改动：%q", got)
	}
}

func jsonNumber(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
