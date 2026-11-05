package app

// LLM 上游凭据的热更与持久化。
//
// 桌面端用户在向导/设置里填 key 的时刻，控制面已经在跑了（同进程），
// 所以「改 key」必须是运行时操作而不是重启参数。落盘用 dataDir 下的
// llm-key.json（0600）：桌面端没有 k8s secret，本地单机文件的权限模型
// 与 jwt-secret 一致——这是刻意的一致，别发明第三种本地凭据存放方式。

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/WangYulong666/astra-agent/internal/api"
	"github.com/WangYulong666/astra-agent/internal/llm"
)

// llmKeyFile 是落盘格式。BaseURL 为空表示用默认上游。
type llmKeyFile struct {
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseUrl,omitempty"`
}

func llmKeyPath(dataDir string) string { return filepath.Join(dataDir, "llm-key.json") }

func loadLLMKeyFile(dataDir string) (llmKeyFile, error) {
	var out llmKeyFile
	b, err := os.ReadFile(llmKeyPath(dataDir))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("llm-key.json 解析失败: %w", err)
	}
	return out, nil
}

func saveLLMKeyFile(dataDir string, f llmKeyFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(llmKeyPath(dataDir), b, 0o600)
}

// llmSettings 实现 api.LLMSettingsStore。
type llmSettings struct {
	srv     *Server
	dataDir string
	lg      *log.Logger
	mu      sync.Mutex
}

func (s *llmSettings) Current() (apiKey, baseURL string, gatewayOn bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, base := s.srv.llmKey, s.srv.llmBase
	return key, base, s.srv.llmOn
}

func (s *llmSettings) Set(apiKey, baseURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.srv.applyGateway(apiKey, defaultIfEmpty(baseURL, s.srv.llmBase))
	s.srv.llmKey, s.srv.llmBase = apiKey, defaultIfEmpty(baseURL, s.srv.llmBase)
	if err := saveLLMKeyFile(s.dataDir, llmKeyFile{APIKey: apiKey, BaseURL: baseURL}); err != nil {
		// 热更已生效、持久化失败：重启后回退到旧 key。这不是静默失败——
		// 返回给前端让用户知道「本次生效但没存下来」。
		return fmt.Errorf("热更已生效，但持久化失败: %w", err)
	}
	s.lg.Printf("LLM 上游凭据已更新（hasKey=%v baseUrl=%s）并落盘", apiKey != "", baseURL)
	return nil
}

func defaultIfEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// applyGateway 用给定的 key/base 重建网关。加锁路径：Set 已持 llmSettings.mu，
// 启动期调用不持锁但单 goroutine，安全。
func (s *Server) applyGateway(key, base string) {
	if base == "" {
		base = DefaultDeepSeekUpstream
	}
	api.SetLLMGateway(llm.NewGateway(
		llm.NewRegistry(llm.Config{
			Providers: map[string]llm.Provider{
				llm.ProviderDeepSeek: {Name: llm.ProviderDeepSeek, BaseURL: base, APIKey: key},
			},
			Default: s.llmProvider,
		}),
		llm.NewPricing(nil),
	))
	s.llmKey, s.llmBase, s.llmOn = key, base, true
	s.lg.Printf("LLM 网关就绪：上游 %s（真 key 只在本进程内存）", base)
}

const DefaultDeepSeekUpstream = "https://api.deepseek.com"

// loadOrCreateSecret 从数据目录读 JWT 密钥，没有就生成并落盘（0600）。
//
// 密钥必须跨重启稳定：换一次密钥 = 所有 access/refresh token 一起失效，
// 用户表现为"莫名其妙又要登录"，且虚拟 LLM key 也一起废掉。
func loadOrCreateSecret(dataDir string, lg *log.Logger) ([]byte, error) {
	p := filepath.Join(dataDir, "jwt-secret")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 32 {
		return b, nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		lg.Printf("⚠ 无法持久化 JWT 密钥（%v）：重启后所有登录态失效", err)
		return buf, nil
	}
	lg.Printf("已生成并持久化 JWT 密钥（%s，0600）", p)
	return buf, nil
}
