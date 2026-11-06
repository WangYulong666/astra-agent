package egress

import "net/url"

// EnvFor 生成注入沙箱的代理环境变量。
//
// 归属信息必须走 userinfo：Go/Python/curl 都会自动把它变成
// `Proxy-Authorization: Basic ...`，沙箱代码不需要知道任何代理细节。
// nonce 由主控生成（每条沙箱一个），这里只拼装、不校验。
//
// 用法（runner 侧）：
//
//	for k, v := range egress.EnvFor(ctl.Addr(), sandboxID, nonce) {
//		env[k] = v
//	}
//
// 这里刻意不设 NO_PROXY：默认严格，所有出网都走代理。需要本机例外时
// 用 EnvForWithBypass 显式传入，别在这里偷偷放行 localhost。
func EnvFor(proxyAddr, sandboxID, nonce string) map[string]string {
	if proxyAddr == "" {
		return nil
	}
	auth := ""
	if sandboxID != "" {
		auth = url.UserPassword(sandboxID, nonce).String() + "@"
	}
	u := "http://" + auth + proxyAddr
	// 大小写都设：Go 只认大写，python-requests/curl 认小写，老工具还认 ALL_PROXY。
	return map[string]string{
		"HTTP_PROXY":  u,
		"HTTPS_PROXY": u,
		"ALL_PROXY":   u,
		"http_proxy":  u,
		"https_proxy": u,
		"all_proxy":   u,
	}
}

// EnvForWithBypass 在 EnvFor 的基础上加 NO_PROXY 例外。
//
// 为什么需要：agent 调模型经常走本机 LLM 网关（127.0.0.1:<port>/v1/llm），
// 而代理默认 deny + 拦私网，全量注入 HTTP_PROXY 会把网关一起挡掉——
// 表现是「开了出网管控之后所有 run 都报模型连不上」。例外值应由主控按
// 实际网关地址精确给出（如 "127.0.0.1:7788"），不要图省事填 "localhost,*"。
//
// 安全说明：这不是「新增后门」。当前 local provider 没有网络命名空间隔离，
// 沙箱代码本来就可以忽略 HTTP_PROXY 直连；真正的强制边界要靠 cube microVM
// 的网桥策略（下一轮）。所以 NO_PROXY 只是让必需的本机流量不绕远路，
// 不应被当成安全边界来看待。
func EnvForWithBypass(proxyAddr, sandboxID, nonce, bypass string) map[string]string {
	env := EnvFor(proxyAddr, sandboxID, nonce)
	if len(env) == 0 || bypass == "" {
		return env
	}
	env["NO_PROXY"] = bypass
	env["no_proxy"] = bypass
	return env
}
