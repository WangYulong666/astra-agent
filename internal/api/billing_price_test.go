package api

// billing 单价回显的测试：响应里的 models 数组必须带上计费时实际用的单价。
//
// 通过 SetLLMGateway 注入真网关（价格表用内置默认价），测完立刻清空——
// gateway 是包级变量，不清会漏进同包其他测试。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/WangYulong666/astra-agent/internal/llm"
	"github.com/WangYulong666/astra-agent/internal/store"
)

func TestBillingIncludesUnitPrices(t *testing.T) {
	p := newPlatformHarness(t)
	_, ownerTok, _, _ := p.seedTenant(t)

	now := time.Now().UTC()
	p.db.usage = append(p.db.usage,
		store.Usage{
			ID: "b1", TenantID: "t1", Model: "deepseek-chat",
			InputTokens: 1_000_000, OutputTokens: 500_000,
			CostMicroUSD: 300_000 + 600_000, Source: "gateway", CreatedAt: now,
		},
		store.Usage{
			ID: "b2", TenantID: "t1", Model: "deepseek-v4-pro",
			InputTokens: 100, OutputTokens: 100,
			CostMicroUSD: 528, Source: "gateway", CreatedAt: now,
		},
	)

	// 网关未启用：modelBilling 不带单价字段。
	code, out := p.do(t, http.MethodGet, "/v1/usage/billing", ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("billing 返回 %d: %s", code, out)
	}
	var bill struct {
		Models []modelBilling `json:"models"`
	}
	if err := json.Unmarshal(out, &bill); err != nil {
		t.Fatal(err)
	}
	if len(bill.Models) != 2 {
		t.Fatalf("应有 2 个模型小计: %s", out)
	}
	for _, m := range bill.Models {
		if m.InputPerMTokMicroUSD != nil || m.OutputPerMTokMicroUSD != nil {
			t.Fatalf("网关未启用时不应有单价: %+v", m)
		}
	}

	// 注入网关：单价必须出现在响应里，且与内置价格表一致。
	llmGateway := llm.NewGateway(nil, llm.NewPricing(nil))
	SetLLMGateway(llmGateway)
	t.Cleanup(func() { SetLLMGateway(nil) })

	code, out = p.do(t, http.MethodGet, "/v1/usage/billing", ownerTok, nil)
	if code != http.StatusOK {
		t.Fatalf("billing 返回 %d: %s", code, out)
	}
	if err := json.Unmarshal(out, &bill); err != nil {
		t.Fatal(err)
	}
	byModel := map[string]modelBilling{}
	for _, m := range bill.Models {
		byModel[m.Model] = m
	}
	chat, ok := byModel["deepseek-chat"]
	if !ok || chat.InputPerMTokMicroUSD == nil || chat.OutputPerMTokMicroUSD == nil {
		t.Fatalf("deepseek-chat 缺单价: %+v", chat)
	}
	if *chat.InputPerMTokMicroUSD != 300_000 || *chat.OutputPerMTokMicroUSD != 1_200_000 {
		t.Fatalf("deepseek-chat 单价与内置价不符: %d / %d",
			*chat.InputPerMTokMicroUSD, *chat.OutputPerMTokMicroUSD)
	}
	pro := byModel["deepseek-v4-pro"]
	if pro.InputPerMTokMicroUSD == nil || *pro.InputPerMTokMicroUSD != 1_320_000 {
		t.Fatalf("deepseek-v4-pro 单价不符: %+v", pro)
	}
}
