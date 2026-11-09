package doclib

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// docEntry 是磁盘文件解析后的不可变快照。缓存按 (mtime,size) 失效。
type docEntry struct {
	mtime    time.Time
	size     int64
	title    string
	summary  string
	category string
	body     string
}

// fileRef 是一次扫描看到的文件。扫描只取元信息，不读内容。
type fileRef struct {
	rel   string // 相对 Root，始终用 /
	mtime time.Time
	size  int64
}

type library struct {
	root         string // 绝对路径；Root 为空时是 ""
	overview     OverviewSource
	overviewPath string
	now          func() time.Time

	mu    sync.RWMutex
	cache map[string]docEntry // key: 相对路径（/）
}

func openImpl(cfg Config) (Library, error) {
	lib := &library{
		overview: cfg.Overview,
		now:      time.Now,
		cache:    make(map[string]docEntry),
	}
	if root := strings.TrimSpace(cfg.Root); root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("doclib: 解析文档根目录: %w", err)
		}
		lib.root = filepath.Clean(abs)
	}
	path := normalizeDocPath(cfg.OverviewPath)
	if path == "" {
		path = "overview"
	}
	lib.overviewPath = path
	return lib, nil
}

// normalizeDocPath 把外部传入的路径收敛成「无前导斜杠、用 / 的相对路径」。
// 越界或空值返回 ""。
func normalizeDocPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	p = strings.TrimPrefix(p, "./")
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

func (l *library) isOverviewPath(p string) bool {
	if l.overview == nil || l.overviewPath == "" {
		return false
	}
	return normalizeDocPath(p) == l.overviewPath
}

// ── Index ───────────────────────────────────────────────────

func (l *library) Index(ctx context.Context) ([]Doc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refs := l.scan(ctx)
	entries := l.resolve(ctx, refs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	docs := make([]Doc, 0, len(refs)+1)
	for _, r := range refs {
		e, ok := entries[r.rel]
		if !ok {
			continue // 读失败的文件跳过，不让整页 500
		}
		docs = append(docs, Doc{
			Path:      r.rel,
			Title:     e.title,
			Category:  e.category,
			Summary:   e.summary,
			UpdatedAt: e.mtime,
		})
	}
	sortDocs(docs)
	if l.overview != nil {
		docs = append([]Doc{{
			Path:      l.overviewPath,
			Title:     "功能总览",
			Category:  "overview",
			Summary:   overviewLead,
			UpdatedAt: l.now(),
			Generated: true,
		}}, docs...)
	}
	return docs, nil
}

// sortDocs 固定分类顺序，同类按标题（strings.Compare，中文按 UTF-8 字节序）。
func sortDocs(docs []Doc) {
	order := func(c string) int {
		if v, ok := categoryOrder[c]; ok {
			return v
		}
		return len(categoryOrder)
	}
	sort.SliceStable(docs, func(i, j int) bool {
		oi, oj := order(docs[i].Category), order(docs[j].Category)
		if oi != oj {
			return oi < oj
		}
		if c := strings.Compare(docs[i].Title, docs[j].Title); c != 0 {
			return c < 0
		}
		return docs[i].Path < docs[j].Path
	})
}

// ── Get ─────────────────────────────────────────────────────

func (l *library) Get(ctx context.Context, path string) (Doc, error) {
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	// 生成页永远实时算：缓存一份「当前能力」三天后就是谎话。
	if l.isOverviewPath(path) {
		return Doc{
			Path:      l.overviewPath,
			Title:     "功能总览",
			Category:  "overview",
			Summary:   overviewLead,
			Body:      renderOverview(ctx, l.overview),
			UpdatedAt: l.now(),
			Generated: true,
		}, nil
	}

	target, rel, err := l.resolvePath(path)
	if err != nil {
		return Doc{}, ErrNotFound
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		return Doc{}, ErrNotFound
	}

	l.mu.RLock()
	e, ok := l.cache[rel]
	l.mu.RUnlock()
	if ok && e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
		return docFromEntry(rel, e), nil
	}

	body, err := os.ReadFile(target)
	if err != nil {
		return Doc{}, ErrNotFound
	}
	// 读的时候文件可能又被改了：以读后的 stat 为准，size 用实际内容长度。
	if fi, serr := os.Stat(target); serr == nil {
		info = fi
	}
	e = entryFrom(rel, string(body), info.ModTime(), int64(len(body)))
	l.mu.Lock()
	l.cache[rel] = e
	l.mu.Unlock()
	return docFromEntry(rel, e), nil
}

func docFromEntry(rel string, e docEntry) Doc {
	return Doc{
		Path:      rel,
		Title:     e.title,
		Category:  e.category,
		Summary:   e.summary,
		Body:      e.body,
		UpdatedAt: e.mtime,
	}
}

func entryFrom(rel, body string, mtime time.Time, size int64) docEntry {
	p := parseDoc(rel, body)
	return docEntry{
		mtime:    mtime,
		size:     size,
		title:    p.title,
		summary:  p.summary,
		category: p.category,
		body:     p.body,
	}
}

// resolvePath 把外部路径变成可信的绝对文件路径。
// 三道闸：绝对路径拒绝、Clean+Rel 防 ../、EvalSymlinks 防软链逃逸。
// 任何异常都回 ErrNotFound，不区分「无权限」和「不存在」，避免被拿来探测目录。
func (l *library) resolvePath(name string) (target, rel string, err error) {
	if l.root == "" {
		return "", "", ErrNotFound
	}
	raw := strings.TrimSpace(name)
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", "", ErrNotFound
	}
	fsPath := filepath.FromSlash(raw)
	if filepath.IsAbs(fsPath) || filepath.VolumeName(fsPath) != "" {
		return "", "", ErrNotFound
	}

	target = filepath.Clean(filepath.Join(l.root, fsPath))
	r, rerr := filepath.Rel(l.root, target)
	if rerr != nil || r == "." || escapes(r) {
		return "", "", ErrNotFound
	}
	if !strings.HasSuffix(strings.ToLower(r), ".md") {
		return "", "", ErrNotFound
	}

	// 软链：把 root 和 target 都解析成真实路径后再复核一次。
	realRoot, rerr := filepath.EvalSymlinks(l.root)
	if rerr != nil {
		return "", "", ErrNotFound
	}
	realTarget, rerr := filepath.EvalSymlinks(target)
	if rerr != nil {
		return "", "", ErrNotFound
	}
	rr, rerr := filepath.Rel(realRoot, realTarget)
	if rerr != nil || rr == "." || filepath.IsAbs(rr) || escapes(rr) {
		return "", "", ErrNotFound
	}
	return target, filepath.ToSlash(r), nil
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// ── 扫描与缓存 ──────────────────────────────────────────────

// scan 收集 Root 下所有 .md 的元信息。Root 为空/不存在/不是目录时返回 nil。
func (l *library) scan(ctx context.Context) []fileRef {
	if l.root == "" || ctx.Err() != nil {
		return nil
	}
	info, err := os.Stat(l.root)
	if err != nil || !info.IsDir() {
		return nil
	}

	var refs []fileRef
	_ = filepath.WalkDir(l.root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if p == l.root {
				return nil
			}
			if name == "node_modules" || name == "third_party" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(l.root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			// 只索引指向根内的软链，根外内容不能漏进列表。
			if _, _, derr := l.resolvePath(relSlash); derr != nil {
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		// 软链用 Stat 跟到目标，保证缓存键和 Get 看到的是同一个文件。
		fi, ferr := os.Stat(p)
		if ferr != nil || !fi.Mode().IsRegular() {
			return nil
		}
		refs = append(refs, fileRef{rel: relSlash, mtime: fi.ModTime(), size: fi.Size()})
		return nil
	})
	sort.Slice(refs, func(i, j int) bool { return refs[i].rel < refs[j].rel })
	return refs
}

// resolve 返回 refs 对应的解析结果。命中缓存的不碰磁盘；变化的在锁外读。
func (l *library) resolve(ctx context.Context, refs []fileRef) map[string]docEntry {
	l.mu.RLock()
	cached := make(map[string]docEntry, len(refs))
	for _, r := range refs {
		if e, ok := l.cache[r.rel]; ok {
			cached[r.rel] = e
		}
	}
	l.mu.RUnlock()

	result := make(map[string]docEntry, len(refs))
	stale := make([]fileRef, 0, len(refs))
	for _, r := range refs {
		if e, ok := cached[r.rel]; ok && e.size == r.size && e.mtime.Equal(r.mtime) {
			result[r.rel] = e
			continue
		}
		stale = append(stale, r)
	}

	// 慢活（读盘 + 解析）在锁外做。
	updated := make(map[string]docEntry, len(stale))
	var failed []string
	for _, r := range stale {
		if err := ctx.Err(); err != nil {
			break
		}
		path := filepath.Join(l.root, filepath.FromSlash(r.rel))
		body, err := os.ReadFile(path)
		if err != nil {
			failed = append(failed, r.rel)
			continue
		}
		e := entryFrom(r.rel, string(body), r.mtime, r.size)
		updated[r.rel] = e
		result[r.rel] = e
	}

	l.mu.Lock()
	for rel, e := range updated {
		l.cache[rel] = e
	}
	// 坏文件清掉旧缓存，下轮重试；消失的文件一并回收。
	keep := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		keep[r.rel] = struct{}{}
	}
	for _, rel := range failed {
		delete(l.cache, rel)
	}
	for rel := range l.cache {
		if _, ok := keep[rel]; !ok {
			delete(l.cache, rel)
		}
	}
	l.mu.Unlock()
	return result
}
