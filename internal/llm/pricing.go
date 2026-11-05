package llm

// 价格表与成本计算。
//
// 单位统一用「微美元 / 百万 token」的整数：
//   - 不用 float 存金额：几十万次调用的浮点累加误差会变成账单上的几块钱，
//     而且同一笔账在两次查询里可能算出不同结果（对账时最难受）。
//   - 不用「美元」做最小单位：单次调用常在 $0.0001 量级，以美分为单位会全被截断成 0。
//
// ── 自定义价格文件 ────────────────────────────────────────────
//
// 启动时可从 <data目录>/pricing.json 加载自定义价格（NewPricingFromDir），
// 运行期可经 (*Pricing).Reload 热更新（SIGHUP 由装配层接线）。文件格式：
//
//	{
//	  "comment": "可选说明，解析时忽略",
//	  "prices": {
//	    "deepseek-chat": { "input": 300000, "output": 1200000 },
//	    "deepseek-v4-pro": { "input": 1320000, "output": 3960000 },
//	    "default": { "input": 300000, "output": 1200000 }
//	  }
//	}
//
// 规则：
//   - "prices" 必填；键是模型名（小写、精确匹配 + 最长前缀匹配），值缺一不可；
//   - "default" 是特殊键，覆盖未登记模型的兜底价（不写则沿用内置兜底价）；
//   - input / output 的单位是「微美元 / 百万 token」，必须是 >= 0 的整数；
//   - 命中键按名字整体替换内置价，不做字段合并（半新半旧的价格表比配错还难发现）。
//
// 文件缺失不算错误（用内置默认价）；JSON 非法或数值不合法会返回错误，
// 但 NewPricingFromDir 同时返回内置默认价兜底——装配层记日志后可以照常启动，
// 不该因为一份写坏的价格文件把整个控制面拖死。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MicroPerUSD 1 美元 = 1,000,000 微美元。与 store.Usage.CostMicroUSD 同一单位。
const MicroPerUSD = 1_000_000

// TokenUnit 价格的计量单位：每百万 token。
const TokenUnit = 1_000_000

// PricingFileName 数据目录下的自定义价格文件名。
const PricingFileName = "pricing.json"

// Price 一个模型的单价。
type Price struct {
	InputPerMTokMicroUSD  int64 // 每百万输入 token 的微美元
	OutputPerMTokMicroUSD int64 // 每百万输出 token 的微美元
}

// Cost 计算一次调用的成本（微美元）。
//
// 四舍五入而不是向下截断：单次调用只有几百 token 时，截断会把成本算成 0；
// 百万次调用累积下来就是系统性少算。
//
// 溢出：int64 上限 9.2e18，输入 1e9 token × 1e7 微美元/Mtok 也只有 1e16，
// 远在安全范围内，所以不做饱和处理。
func Cost(p Price, in, out int64) int64 {
	if in < 0 {
		in = 0
	}
	if out < 0 {
		out = 0
	}
	raw := in*p.InputPerMTokMicroUSD + out*p.OutputPerMTokMicroUSD
	return (raw + TokenUnit/2) / TokenUnit
}

// 内置默认单价（微美元 / 百万 token）。
//
// 来源：https://api-docs.deepseek.com/quick_start/pricing，抓取日期 2026-10-30
// （中文页同价：https://api-docs.deepseek.com/zh-cn/quick_start/pricing）。
//
// 官方当前模型是 deepseek-flash 与 deepseek-v4-pro；deepseek-chat /
// deepseek-reasoner 这类旧模型名仍可调用，请求由 DeepSeek-V4.1-Flash 服务、
// 按 Flash 价计费（官方迁移说明同页）。本表按 Flash 的「高峰时段 + 缓存未命中」
// 输入 $0.30/M、输出 $1.20/M 计——缓存命中（$0.006/M）与空闲时段（半价）都更便宜，
// 即本表是计费上界，不会少收。deepseek-v4-pro：输入 $1.32/M、输出 $3.96/M。
// 上游改价请核对该页并更新这里，或用 pricing.json 覆盖。
const (
	flashInputMicroUSD  int64 = 300_000   // $0.30 / Mtok（高峰、缓存未命中）
	flashOutputMicroUSD int64 = 1_200_000 // $1.20 / Mtok（高峰）
	v4ProInputMicroUSD  int64 = 1_320_000 // $1.32 / Mtok（高峰、缓存未命中）
	v4ProOutputMicroUSD int64 = 3_960_000 // $3.96 / Mtok（高峰）
)

// defaultTable 内置的模型 → 单价。
//
// key 既是精确匹配也参与前缀匹配（见 Pricing.Price），所以 "deepseek-reasoner"
// 能覆盖 "deepseek-reasoner-0711" 这类带后缀的型号。
func defaultTable() map[string]Price {
	return map[string]Price{
		"deepseek-chat":     {InputPerMTokMicroUSD: flashInputMicroUSD, OutputPerMTokMicroUSD: flashOutputMicroUSD},
		"deepseek-reasoner": {InputPerMTokMicroUSD: flashInputMicroUSD, OutputPerMTokMicroUSD: flashOutputMicroUSD},
		"deepseek-flash":    {InputPerMTokMicroUSD: flashInputMicroUSD, OutputPerMTokMicroUSD: flashOutputMicroUSD},
		"deepseek-v4-pro":   {InputPerMTokMicroUSD: v4ProInputMicroUSD, OutputPerMTokMicroUSD: v4ProOutputMicroUSD},
		// 兜底：任何没登记的型号都按 Flash 价算，而不是按 0 算。
		// 按 0 算等于「悄悄不收费」，对账时看不出来是漏配还是免费。
		"": {InputPerMTokMicroUSD: flashInputMicroUSD, OutputPerMTokMicroUSD: flashOutputMicroUSD},
	}
}

// Pricing 价格表。读走 RLock，写（Set / Reload）走 Lock，可并发使用。
type Pricing struct {
	mu    sync.RWMutex
	table map[string]Price
}

// defaultKey 是兜底价的内部键。Price() 未命中时查它。
const defaultKey = ""

// DefaultOverrideName 价格文件里覆盖兜底价的特殊键（见文件头注释）。
// NewPricing 的 overrides 参数同样接受它。
const DefaultOverrideName = "default"

// NewPricing 用内置价格表 + 配置覆盖生成价格表。
//
// 覆盖项按名字整体替换，不做字段合并：只改输出价却忘改输入价时，
// 半新半旧的价格表比配错还难发现。
func NewPricing(overrides map[string]Price) *Pricing {
	t := defaultTable()
	applyOverrides(t, overrides)
	return &Pricing{table: t}
}

// applyOverrides 把覆盖项写进表。"default" 映射到内部兜底键，
// 其余名字统一小写去空格。调用方保证 dst 非 nil。
func applyOverrides(dst map[string]Price, overrides map[string]Price) {
	for k, v := range overrides {
		name := strings.ToLower(strings.TrimSpace(k))
		if name == DefaultOverrideName {
			name = defaultKey
		}
		dst[name] = v
	}
}

// NewPricingFromDir 从数据目录加载 <dataDir>/pricing.json 并生成价格表。
//
// 返回值永远可用：文件缺失或内容非法时返回内置默认价 + 相应错误，
// 装配层记日志后可以照常启动——一份写坏的价格文件不该拖死控制面，
// 但错误必须显式报出来，否则「自定义价没生效」根本没人知道。
func NewPricingFromDir(dataDir string) (*Pricing, error) {
	if strings.TrimSpace(dataDir) == "" {
		return NewPricing(nil), nil
	}
	return NewPricingFromFile(filepath.Join(dataDir, PricingFileName))
}

// NewPricingFromFile 从指定路径加载自定义价格。
func NewPricingFromFile(path string) (*Pricing, error) {
	overrides, err := LoadPricingFile(path)
	if err != nil {
		return NewPricing(nil), err
	}
	return NewPricing(overrides), nil
}

// LoadPricingFile 解析价格文件，返回覆盖项。
//
// 文件不存在返回 (nil, nil)——「没配」是正常态，不是错误。
func LoadPricingFile(path string) (map[string]Price, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("llm: 读取价格文件 %s 失败: %w", path, err)
	}

	var doc struct {
		Comment string `json:"comment"`
		Prices  map[string]struct {
			Input  *int64 `json:"input"`
			Output *int64 `json:"output"`
		} `json:"prices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("llm: 价格文件 %s 不是合法 JSON: %w", path, err)
	}
	if len(doc.Prices) == 0 {
		return nil, fmt.Errorf("llm: 价格文件 %s 缺少 prices 字段", path)
	}

	out := make(map[string]Price, len(doc.Prices))
	for name, p := range doc.Prices {
		if p.Input == nil || p.Output == nil {
			return nil, fmt.Errorf("llm: 价格文件 %s 中 %q 缺少 input 或 output", path, name)
		}
		if *p.Input < 0 || *p.Output < 0 {
			return nil, fmt.Errorf("llm: 价格文件 %s 中 %q 的价格不能为负数", path, name)
		}
		out[name] = Price{InputPerMTokMicroUSD: *p.Input, OutputPerMTokMicroUSD: *p.Output}
	}
	return out, nil
}

// Reload 重新加载价格文件并整体替换当前表（SIGHUP 热更新用）。
//
// 解析失败时保持旧表不变并返回错误：改价失败不该把正在服务的价格表清空。
// 调用方（装配层）负责把返回的错误打进日志。
func (p *Pricing) Reload(path string) error {
	overrides, err := LoadPricingFile(path)
	if err != nil {
		return err
	}
	t := defaultTable()
	applyOverrides(t, overrides)
	p.mu.Lock()
	p.table = t
	p.mu.Unlock()
	return nil
}

// Price 查单价。第二个返回值表示「是否命中显式配置」，
// 未命中时返回兜底价而不是零值——调用方据此可以在日志里提示漏配。
func (p *Pricing) Price(model string) (Price, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.table == nil {
		return Price{}, false
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if v, ok := p.table[m]; ok {
		return v, true
	}
	// 前缀匹配取最长者：型号常带日期后缀，但 "deepseek-chat" 不能吃掉 "deepseek-chatter"。
	bestLen := -1
	var best Price
	for k, v := range p.table {
		if k == "" || !strings.HasPrefix(m, k) {
			continue
		}
		if len(k) > bestLen {
			bestLen, best = len(k), v
		}
	}
	if bestLen >= 0 {
		return best, true
	}
	return p.table[""], false
}

// Set 运行期改单项价格。整体换表用 Reload。
func (p *Pricing) Set(model string, v Price) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.table == nil {
		p.table = defaultTable()
	}
	name := strings.ToLower(strings.TrimSpace(model))
	if name == DefaultOverrideName {
		name = defaultKey
	}
	p.table[name] = v
}
