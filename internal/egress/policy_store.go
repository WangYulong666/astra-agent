package egress

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// policyFileName 是租户策略的落盘文件。
//
// 放在 DataDir 根下（不是子目录）：这个文件只有一份、很小、需要被人直接
// 打开排查，埋一层目录反而难找。
const policyFileName = "egress_policy.json"

// errBadTarget 表示 CONNECT/absolute-URI 里的目标地址解析不出来。
var errBadTarget = &targetError{}

type targetError struct{}

func (*targetError) Error() string { return "egress: 目标地址非法" }

// errPrivateAddress 是拨号器在 DNS 解析之后拦下私网地址时返回的哨兵错误，
// 便于区分「策略拒绝」和「拨号失败」。
var errPrivateAddress = &privateError{}

type privateError struct{}

func (*privateError) Error() string { return "egress: 目标是私网/链路本地地址" }

func isValidDecision(d Decision) bool {
	switch d {
	case Allow, Deny, Ask:
		return true
	}
	return false
}

// normalizePolicy 做两件事：补默认值（Default 为空按 deny）、校验非法规则。
// 校验放在这里而不是只依赖 API 层，是因为 Controller 可能被其它内部调用方
// 直接使用，坏策略一旦进了内存，每次请求都要重新解释。
func normalizePolicy(p Policy) (Policy, error) {
	out := Policy{
		Default:      p.Default,
		AllowPrivate: p.AllowPrivate,
		LogBody:      p.LogBody,
		Rules:        make([]Rule, 0, len(p.Rules)),
	}
	if out.Default == "" {
		out.Default = Deny
	}
	if !isValidDecision(out.Default) {
		return Policy{}, fmt.Errorf("egress: 非法的默认决策 %q", out.Default)
	}
	for i, r := range p.Rules {
		pat := strings.TrimSpace(r.Pattern)
		if pat == "" {
			return Policy{}, fmt.Errorf("egress: 第 %d 条规则缺少 pattern", i+1)
		}
		if !isValidDecision(r.Decision) {
			return Policy{}, fmt.Errorf("egress: 规则 %q 的决策 %q 非法", pat, r.Decision)
		}
		out.Rules = append(out.Rules, Rule{Pattern: pat, Decision: r.Decision, Note: r.Note})
	}
	return out, nil
}

func clonePolicy(p Policy) Policy {
	out := p
	if p.Rules != nil {
		out.Rules = append([]Rule(nil), p.Rules...)
	}
	return out
}

// decideHost 是策略判定的唯一入口。顺序很关键：
//
//  1. 私网拦截优先于任何规则——「allow *」不能把 169.254.169.254 放出去；
//  2. 规则按顺序，第一条命中即生效（Rule 注释里承诺过）；
//  3. Ask 目前按 Deny 处理并标 ask-unsupported（审批流下一轮接）；
//  4. 都没命中走 Default。
func decideHost(pol Policy, host string) (Decision, string) {
	host = normalizeHost(host)
	if host == "" {
		return Deny, "bad-target"
	}
	if !pol.AllowPrivate {
		if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
			return Deny, "private-address"
		}
	}
	for _, rule := range pol.Rules {
		if !matchPattern(rule.Pattern, host) {
			continue
		}
		switch rule.Decision {
		case Allow:
			return Allow, "rule:" + rule.Pattern
		case Deny:
			return Deny, "rule:" + rule.Pattern
		case Ask:
			// 没有审批通道时放行是不负责任的，挂起又没人会来批，
			// 所以只能是「拒绝 + 显式原因」，让用户知道这不是配置错了。
			return Deny, "ask-unsupported"
		}
	}
	if pol.Default == Allow {
		return Allow, "default"
	}
	if pol.Default == Ask {
		return Deny, "ask-unsupported"
	}
	return Deny, "default"
}

// policyFile 是磁盘格式。带 version 是为了下一轮改结构时能选中迁移路径。
type policyFile struct {
	Version int               `json:"version"`
	Tenants map[string]Policy `json:"tenants"`
}

// dataDir 解析策略落盘目录。
//
// Config 里没有 DataDir（egress.go 已冻结，不能加字段），所以按优先级取
// 环境变量，最后退回控制面的默认数据目录。主控若愿意，改为把 app.Config.DataDir
// 透传进来即可删掉这段推断。
func dataDir() string {
	for _, k := range []string{"EGRESS_DATA_DIR", "ASTRA_DATA"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".astra", "data")
}

// loadPolicies 读租户策略。文件不存在属于正常（首次启动）；文件损坏时
// 不阻止代理启动——出网管控宁可先按默认 deny 跑起来，也不能因为一个坏 JSON
// 让整个控制面起不来。
func (p *proxy) loadPolicies() {
	p.policies = make(map[string]Policy)
	if p.policyPath == "" {
		return
	}
	raw, err := os.ReadFile(p.policyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			p.logf("读取出网策略 %s 失败: %v（按默认策略启动）", p.policyPath, err)
		}
		return
	}
	var f policyFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Tenants == nil {
		// 兼容早期/手写成的 map[string]Policy
		var legacy map[string]Policy
		if err2 := json.Unmarshal(raw, &legacy); err2 != nil {
			p.logf("解析出网策略 %s 失败: %v（按默认策略启动）", p.policyPath, err)
			return
		}
		f.Tenants = legacy
	}
	for tid, pol := range f.Tenants {
		np, err := normalizePolicy(pol)
		if err != nil {
			p.logf("跳过租户 %s 的非法出网策略: %v", tid, err)
			continue
		}
		p.policies[tid] = np
	}
}

// persist 把当前策略表整份写盘（临时文件 + rename，0600）。
// 只有 SetPolicy 会调它，不涉及每请求路径。
func (p *proxy) persist(snapshot map[string]Policy) error {
	if p.policyPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.policyPath), 0o700); err != nil {
		return fmt.Errorf("egress: 创建策略目录: %w", err)
	}
	body, err := json.MarshalIndent(policyFile{Version: 1, Tenants: snapshot}, "", "  ")
	if err != nil {
		return fmt.Errorf("egress: 编码策略: %w", err)
	}
	body = append(body, '\n')
	tmp := p.policyPath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("egress: 写策略临时文件: %w", err)
	}
	if err := os.Rename(tmp, p.policyPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("egress: 落盘策略: %w", err)
	}
	return nil
}
