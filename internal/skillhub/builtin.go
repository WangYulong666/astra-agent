package skillhub

import (
	"embed"
	"io/fs"
)

// 内置 skill 打进二进制，而不是在运行时去仓库 skills/ 里翻——
// 装配方可能把二进制拷到任意 cwd，甚至没有源码树，依赖 cwd 会让
// 「启动时同步内置」变成「有时同步、有时不发布」的随机行为。
//
// 只嵌 SKILL.md：examples.md 之类的资源属于渐进式披露第三层，
// 由调用方按需从它自己的目录读，这里不替它决定要带哪些资源。
//
//go:embed builtin/*/SKILL.md
var builtinFS embed.FS

// builtinSkills 返回内置 skill 名 -> SKILL.md 全文。
func builtinSkills() (map[string][]byte, error) {
	ents, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := builtinFS.ReadFile("builtin/" + e.Name() + "/SKILL.md")
		if err != nil {
			return nil, err
		}
		out[e.Name()] = b
	}
	return out, nil
}
