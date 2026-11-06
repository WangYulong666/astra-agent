package app

// 极简 .env 加载。不引第三方库的原因：只需要「KEY=VALUE + 注释 + 引号」这四件事，
// 而 godotenv 会顺带引入它的变量展开与多文件优先级规则——那些行为我们不需要，
// 出了差异反而难查。
//
// 语义刻意定得很窄：
//   - 只补**未设置**的变量（shell 里 export 的优先，方便临时覆盖）
//   - 不展开 ${VAR}（避免「看着像 shell 其实是字面量」的误解）
//   - 成对引号会被去掉，引号外的行尾「 # 注释」会被剥掉
//   - 文件不存在是正常情况，不报错
//
// 为什么必须剥行尾注释：.env.example 里大量写成
//
//	MILVUS_ADDR=   # 127.0.0.1:19530；留空=关闭
//
// 用户照惯例 cp 一份就跑，如果不剥，MILVUS_ADDR 的值就是那串中文注释——
// 能力被「启用」到一个根本不存在的地址上，而 /v1/meta 显示 on，
// 排查时没人会想到是 .env 的注释。值里本身含 #（密码、URL fragment）
// 要写成引号包住，引号内的 # 不处理。

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
		quoted := false
		if len(v) >= 2 {
			if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
				quoted = true
			}
		}
		// 未加引号时，「空白 + #」后面的都是注释。要求有空白是为了不误伤
		// 形如 key=abc#def 的值——那种 # 是内容，不是注释。
		// TrimSpace 之后整行以 # 开头（KEY=   # 注释）也算纯注释，值是空串。
		if !quoted {
			if strings.HasPrefix(v, "#") {
				v = ""
			} else if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
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
