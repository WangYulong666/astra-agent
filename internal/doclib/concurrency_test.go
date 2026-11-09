package doclib

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentIndexGetSearch(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		writeDoc(t, root, fmt.Sprintf("d%d.md", i),
			fmt.Sprintf("# Doc %d\n\n并发正文 alpha-%d。\n", i, i))
	}
	src := stubOverview{
		features: testFeatures,
		meta:     map[string]string{"provider": "deepseek", "agent": "astra"},
	}
	lib := openLib(t, Config{Root: root, Overview: src})
	ctx := context.Background()

	const (
		workers = 8
		rounds  = 40
	)
	var wg sync.WaitGroup
	var failures atomic.Int32

	fail := func(format string, args ...any) {
		failures.Add(1)
		t.Errorf(format, args...)
	}

	// 一个写者持续改文件，逼缓存失效路径与读者交错。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			path := filepath.Join(root, "d0.md")
			body := fmt.Sprintf("# Doc 0\n\n改到第 %d 版 alpha。\n", i)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				fail("写入失败: %v", err)
				return
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				docs, err := lib.Index(ctx)
				if err != nil {
					fail("Index: %v", err)
					return
				}
				if len(docs) != 9 { // 8 篇 + 总览
					fail("Index 返回 %d 篇", len(docs))
					return
				}
				if !docs[0].Generated {
					fail("总览应固定在最前")
					return
				}

				doc, err := lib.Get(ctx, fmt.Sprintf("d%d.md", (w+i)%8))
				if err != nil {
					fail("Get: %v", err)
					return
				}
				if doc.Body == "" && doc.Title == "" {
					fail("Get 返回空文档")
					return
				}

				hits, err := lib.Search(ctx, "alpha", 5)
				if err != nil {
					fail("Search: %v", err)
					return
				}
				if len(hits) == 0 {
					fail("Search 没有命中")
					return
				}
				for _, h := range hits {
					if !strings.Contains(strings.ToLower(h.Summary), "alpha") {
						fail("搜索摘要未定位命中: %q", h.Summary)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("并发访问出现 %d 次失败", n)
	}
}
