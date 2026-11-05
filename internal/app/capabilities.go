package app

// 可选能力装配：Milvus 长期记忆、Langfuse 观测。
//
// 原则：**配了就开，起不来只记日志**。平台的核心价值是「能起沙箱跑 agent」，
// 记忆和观测是增强项——不能因为一台机器没装 Milvus 就整个起不来。
// 能力开关会写进 /v1/meta，前端和 CLI 据此决定是否显示入口。

import (
	"log"
	"os"
	"strconv"

	"github.com/WangYulong666/astra-agent/internal/memory"
	"github.com/WangYulong666/astra-agent/internal/trace"
)

type capInfo struct {
	Endpoint     string
	DashboardURL string
	PublicKey    string
}

func openCapabilities(lg *log.Logger) (memory.Store, trace.Tracer, capInfo) {
	var info capInfo

	memStore, err := memory.Open(memory.Config{
		Addr:         os.Getenv("MILVUS_ADDR"),
		DB:           envOr("MILVUS_DB", "default"),
		Collection:   envOr("MILVUS_COLLECTION", "astra_memory"),
		EmbedBaseURL: os.Getenv("MEMORY_EMBED_BASE_URL"),
		EmbedAPIKey:  os.Getenv("MEMORY_EMBED_API_KEY"),
		EmbedModel:   envOr("MEMORY_EMBED_MODEL", "text-embedding-3-small"),
		Dim:          envInt("MEMORY_EMBED_DIM", 0),
	})
	switch {
	case err != nil:
		lg.Printf("长期记忆未启用: %v", err)
		memStore = memory.Disabled()
	case memory.IsDisabled(memStore):
		// 没配 MILVUS_ADDR：静默关闭，不刷日志
	default:
		lg.Printf("长期记忆已启用: Milvus %s (collection=%s)", os.Getenv("MILVUS_ADDR"), envOr("MILVUS_COLLECTION", "astra_memory"))
	}

	tracer, err := trace.Open(trace.Config{
		Endpoint:   os.Getenv("LANGFUSE_HOST"),
		PublicKey:  os.Getenv("LANGFUSE_PUBLIC_KEY"),
		SecretKey:  os.Getenv("LANGFUSE_SECRET_KEY"),
		SampleRate: envFloat("LANGFUSE_SAMPLE_RATE", 1),
	})
	if err != nil {
		lg.Printf("观测未启用: %v", err)
		tracer = trace.Disabled()
	} else if tracer.Enabled() {
		lg.Printf("观测已启用: Langfuse %s", os.Getenv("LANGFUSE_HOST"))
	}

	info.Endpoint = os.Getenv("LANGFUSE_HOST")
	info.PublicKey = os.Getenv("LANGFUSE_PUBLIC_KEY")
	// 自建 Langfuse 的 dashboard 通常就在 host 上；托管版可用这个变量指到云控制台
	info.DashboardURL = envOr("LANGFUSE_DASHBOARD_URL", info.Endpoint)
	return memStore, tracer, info
}

func boolLabel(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
