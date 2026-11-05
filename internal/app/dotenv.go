package app

// 极简 .env 加载。不引第三方库的原因：只需要「KEY=VALUE + 注释 + 引号」这四件事，
// 而 godotenv 会顺带引入它的变量展开与多文件优先级规则——那些行为我们不需要，
// 出了差异反而难查。
//
// 语义刻意定得很窄：
//   - 只补**未设置**的变量（shell 里 export 的优先，方便临时覆盖）
//   - 不展开 ${VAR}（避免「看着像 shell 其实是字面量」的误解）
//   - 成对引号会被去掉，其余原样保留（含 # 之后的内容）
//   - 文件不存在是正常情况，不报错

import (
	"bufio"
	"log"
	"os"
	"strings"
)

// LoadDotEnv 读取 path 并补齐环境变量。返回实际加载的条目数。
func LoadDotEnv(path string, lg *log.Logger) int {
	f, err := os.Open(path)
	if err != nil {
		return 0 // 没有 .env 是最常见的情况，不该刷日志
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 兼容 `export KEY=VALUE` 写法
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		if _, exists := os.LookupEnv(k); exists {
			continue // shell 已设置，保留它
		}
		if err := os.Setenv(k, v); err != nil {
			continue
		}
		n++
	}
	if err := sc.Err(); err != nil && lg != nil {
		lg.Printf(".env 读取不完整: %v", err)
	}
	return n
}
