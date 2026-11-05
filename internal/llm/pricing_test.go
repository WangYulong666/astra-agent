package llm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCostRoundsToNearestMicroUSD(t *testing.T) {
	p := Price{InputPerMTokMicroUSD: 300_000, OutputPerMTokMicroUSD: 1_200_000}

	cases := []struct {
		name    string
		in, out int64
		want    int64
	}{
		{"零用量", 0, 0, 0},
		{"手算 1200/340", 1200, 340, 768},
		{"一百万输入", 1_000_000, 0, 300_000},
		{"小到不足 1 微美元也要向上取整", 1, 0, 0}, // 300000/1e6=0.3 → 四舍五入为 0
		{"负数按 0 处理", -5, -5, 0},
	}
	for _, c := range cases {
		if got := Cost(p, c.in, c.out); got != c.want {
			t.Errorf("%s: Cost(%d,%d)=%d，期望 %d", c.name, c.in, c.out, got, c.want)
		}
	}

	// 0.6 微美元必须进位成 1：向下截断会让「每次调用都少算一点点」累积成系统性偏差。
	if got := Cost(Price{InputPerMTokMicroUSD: 600_000}, 1, 0); got != 1 {
		t.Errorf("0.6 微美元应进位为 1，实际 %d", got)
	}
}

func TestPricingFallsBackNotZero(t *testing.T) {
	p := NewPricing(nil)

	if _, ok := p.Price("deepseek-chat"); !ok {
		t.Fatal("内置应包含 deepseek-chat")
	}
	// 带日期后缀的型号走前缀匹配。
	if _, ok := p.Price("deepseek-chat-0711"); !ok {
		t.Fatal("带后缀的型号应命中前缀规则")
	}
	// 完全没登记的型号不能按 0 收费：那等于悄悄免费。
	pr, ok := p.Price("some-unknown-model")
	if ok {
		t.Fatal("未登记型号不该报「命中显式配置」")
	}
	if pr.InputPerMTokMicroUSD == 0 || pr.OutputPerMTokMicroUSD == 0 {
		t.Fatal("未登记型号必须走兜底价而不是 0")
	}

	// 配置覆盖。
	ov := NewPricing(map[string]Price{"deepseek-chat": {InputPerMTokMicroUSD: 1, OutputPerMTokMicroUSD: 2}})
	got, _ := ov.Price("deepseek-chat")
	if got.InputPerMTokMicroUSD != 1 || got.OutputPerMTokMicroUSD != 2 {
		t.Fatalf("配置未覆盖内置价: %+v", got)
	}
}

func TestRegistryDefaultsAndModels(t *testing.T) {
	// 只给一个 deepseek key，也应该能跑（内置 base_url）。
	r := NewRegistry(Config{Providers: map[string]Provider{
		ProviderDeepSeek: {APIKey: "sk-x"},
	}})
	p, err := r.Lookup("")
	if err != nil {
		t.Fatalf("默认 provider 应可用: %v", err)
	}
	if p.BaseURL != DefaultDeepSeekBaseURL {
		t.Fatalf("应回落到内置 base_url，实际 %q", p.BaseURL)
	}
	if got := r.ProviderFor("deepseek-reasoner"); got != ProviderDeepSeek {
		t.Fatalf("deepseek 前缀应映射到 deepseek，实际 %q", got)
	}

	// 没有 key 时必须明确报错，而不是拿空 key 去请上游。
	noKey := NewRegistry(Config{})
	if _, err := noKey.Lookup(ProviderDeepSeek); err == nil {
		t.Fatal("没配 key 时应返回 ErrNoAPIKey")
	}

	// 多 provider + 显式默认。
	multi := NewRegistry(Config{
		Providers: map[string]Provider{
			"deepseek": {APIKey: "sk-1"},
			"openai":   {BaseURL: "https://api.openai.com", APIKey: "sk-2"},
		},
		Default: "openai",
	})
	if got := multi.Default(); got != "openai" {
		t.Fatalf("显式默认应生效，实际 %q", got)
	}
	if got := multi.ProviderFor("deepseek-chat"); got != "deepseek" {
		t.Fatalf("模型前缀应优先于默认值，实际 %q", got)
	}
}

// TestPricingFromDirCustomOverrides 自定义价格文件覆盖内置价，未覆盖的保持内置。
func TestPricingFromDirCustomOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PricingFileName)
	content := `{
		"comment": "测试价格",
		"prices": {
			"deepseek-chat": { "input": 111, "output": 222 },
			"default": { "input": 3, "output": 4 }
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := NewPricingFromDir(dir)
	if err != nil {
		t.Fatalf("加载不应报错: %v", err)
	}
	got, ok := p.Price("deepseek-chat")
	if !ok || got.InputPerMTokMicroUSD != 111 || got.OutputPerMTokMicroUSD != 222 {
		t.Fatalf("自定义价未生效: %+v ok=%v", got, ok)
	}
	// 未覆盖的模型保持内置价。
	if got, _ := p.Price("deepseek-v4-pro"); got.InputPerMTokMicroUSD != v4ProInputMicroUSD {
		t.Fatalf("未覆盖模型应保持内置价，实际 %+v", got)
	}
	// default 覆盖兜底价：未登记型号不再走内置兜底。
	got, ok = p.Price("never-seen-model")
	if ok {
		t.Fatal("未登记型号不该报「命中显式配置」")
	}
	if got.InputPerMTokMicroUSD != 3 || got.OutputPerMTokMicroUSD != 4 {
		t.Fatalf("default 未覆盖兜底价: %+v", got)
	}

	// Reload 热更新：换文件后表整体替换，旧覆盖项失效。
	if err := os.WriteFile(path, []byte(`{"prices":{"deepseek-chat":{"input":9,"output":9}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(path); err != nil {
		t.Fatalf("Reload 失败: %v", err)
	}
	if got, _ := p.Price("deepseek-chat"); got.InputPerMTokMicroUSD != 9 {
		t.Fatalf("Reload 后新价未生效: %+v", got)
	}
	if got, _ := p.Price("never-seen-model"); got.InputPerMTokMicroUSD != flashInputMicroUSD {
		t.Fatalf("Reload 后兜底价应回到内置: %+v", got)
	}
}

// TestPricingFromDirMissingFile 文件缺失是正常态：内置默认价，无错误。
func TestPricingFromDirMissingFile(t *testing.T) {
	p, err := NewPricingFromDir(t.TempDir())
	if err != nil {
		t.Fatalf("缺失文件不应报错: %v", err)
	}
	if _, ok := p.Price("deepseek-chat"); !ok {
		t.Fatal("缺失文件时应回落到内置价")
	}
	// 空 dataDir 同样安全。
	if _, err := NewPricingFromDir(""); err != nil {
		t.Fatalf("空 dataDir 不应报错: %v", err)
	}
}

// TestPricingFromDirInvalidJSON 非法 JSON：必须报错且不能崩，
// 返回的价格表仍然可用（内置默认价兜底）。
func TestPricingFromDirInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PricingFileName)
	if err := os.WriteFile(path, []byte(`{"prices": {oops`), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := NewPricingFromDir(dir)
	if err == nil {
		t.Fatal("非法 JSON 必须返回错误")
	}
	if !strings.Contains(err.Error(), PricingFileName) {
		t.Fatalf("错误信息应指出文件: %v", err)
	}
	// 关键：返回值可用，装配层记日志后可以照常启动。
	if p == nil {
		t.Fatal("出错时也应返回可用的默认价格表")
	}
	if _, ok := p.Price("deepseek-chat"); !ok {
		t.Fatal("出错时的价格表应含内置价")
	}

	// 结构不对同样报错：prices 缺失、字段缺失、负数。
	for name, content := range map[string]string{
		"缺 prices": `{"comment": "啥也没有"}`,
		"缺字段":      `{"prices":{"deepseek-chat":{"input":1}}}`,
		"负数":       `{"prices":{"deepseek-chat":{"input":-1,"output":1}}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPricingFromFile(path); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

// TestDefaultPricesAreRealDeepSeekPrices 内置价必须是查证过的真实价，
// 防止有人随手改回占位数（或清零）。
func TestDefaultPricesAreRealDeepSeekPrices(t *testing.T) {
	p := NewPricing(nil)
	cases := map[string]Price{
		"deepseek-chat":     {flashInputMicroUSD, flashOutputMicroUSD},
		"deepseek-reasoner": {flashInputMicroUSD, flashOutputMicroUSD},
		"deepseek-flash":    {flashInputMicroUSD, flashOutputMicroUSD},
		"deepseek-v4-pro":   {v4ProInputMicroUSD, v4ProOutputMicroUSD},
	}
	for model, want := range cases {
		got, ok := p.Price(model)
		if !ok || got != want {
			t.Fatalf("%s 内置价应为 %+v，实际 %+v（ok=%v）", model, want, got, ok)
		}
	}
}
