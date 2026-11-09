package doclib

import (
	"context"
	"sort"
	"strings"
)

const (
	searchDefaultLimit = 20
	searchMaxLimit     = 100
	searchSnippetPad   = 40 // 命中点前后各 40 个 rune
)

func (l *library) Search(ctx context.Context, query string, limit int) ([]Doc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return []Doc{}, nil
	}
	if limit <= 0 {
		limit = searchDefaultLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}

	refs := l.scan(ctx)
	entries := l.resolve(ctx, refs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	type hit struct {
		doc   Doc
		count int
	}
	needle := strings.ToLower(q)
	hits := make([]hit, 0, len(refs))
	for _, r := range refs {
		e, ok := entries[r.rel]
		if !ok {
			continue
		}
		count := strings.Count(strings.ToLower(e.title), needle) +
			strings.Count(strings.ToLower(e.summary), needle) +
			strings.Count(strings.ToLower(e.body), needle)
		if count == 0 {
			continue
		}
		hits = append(hits, hit{
			doc: Doc{
				Path:      r.rel,
				Title:     e.title,
				Category:  e.category,
				Summary:   matchSummary(e, q),
				UpdatedAt: e.mtime,
			},
			count: count,
		})
	}

	// 命中次数多的在前；同次数按标题，再按路径，保证结果稳定。
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].count != hits[j].count {
			return hits[i].count > hits[j].count
		}
		if c := strings.Compare(hits[i].doc.Title, hits[j].doc.Title); c != 0 {
			return c < 0
		}
		return hits[i].doc.Path < hits[j].doc.Path
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}

	out := make([]Doc, len(hits))
	for i := range hits {
		out[i] = hits[i].doc
	}
	return out, nil
}

// matchSummary 给搜索结果一段「为什么命中」的定位摘要。
// 优先正文（正文命中通常才是真命中），再退到摘要/标题。
func matchSummary(e docEntry, query string) string {
	sources := []string{e.body, e.summary, e.title}
	for _, src := range sources {
		text := collapseSpace(stripMarkdown(src))
		if s := snippetAround(text, query, searchSnippetPad); s != "" {
			return s
		}
	}
	return truncateRunes(e.summary, summaryLimit)
}
