SHELL := /bin/bash
.DEFAULT_GOAL := help

BIN       := bin
# 默认 agent 命令。本地开发可覆盖：make m0 AGENT="node /path/to/dsh/lib/bin.js --profile acp"
AGENT     ?= dsh --profile acp
PROXY_URL ?= ws://127.0.0.1:8080/acp
CWD       ?= /tmp/astra-workspace

.PHONY: help
help: ## 显示所有目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ── 构建与质量 ────────────────────────────────────────────────

.PHONY: build
build: ## 编译全部二进制到 bin/
	@mkdir -p $(BIN)
	go build -o $(BIN)/acp-proxy ./cmd/acp-proxy
	go build -o $(BIN)/acp-smoke ./cmd/acp-smoke
	go build -o $(BIN)/acp-mock-agent ./cmd/acp-mock-agent
	go build -o $(BIN)/acp-shell-agent ./cmd/acp-shell-agent
	go build -o $(BIN)/astra-sandbox ./cmd/astra-sandbox
	go build -o $(BIN)/astra-server ./cmd/astra-server
	@ls -lh $(BIN)/

# ── 前端 ──────────────────────────────────────────────────────
#
# 产物写到 internal/api/web/dist，由 go:embed 打进 astra-server。
# 因此顺序是「先 make web 再 make build」；dist 已提交，直接 build 也能跑。

WEB := web
NPM ?= npm

.PHONY: web
web: ## 构建前端并嵌入（需要 node ≥ 20）
	cd $(WEB) && $(NPM) install --no-audit --no-fund
	cd $(WEB) && $(NPM) run build
	@echo "→ 产物已写入 internal/api/web/dist"

.PHONY: web-dev
web-dev: ## 前端热更新（另开一个终端跑 make serve）
	cd $(WEB) && $(NPM) run dev

.PHONY: test
test: ## 跑单元测试（不依赖 agent / 网络）
	go test ./... -count=1 -timeout 120s

.PHONY: e2e
e2e: ## 端到端冒烟：起服务 → 向导 → 建会话 → 跑任务 → 完成
	bash scripts/e2e.sh

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: fmt
fmt: ## 格式化
	gofmt -l -w $$(find . -name '*.go' -not -path './third_party/*' -not -path './vendor/*')

.PHONY: tidy
tidy: ## 整理依赖
	go mod tidy
	go mod vendor

# ── M0 验证 ───────────────────────────────────────────────────

.PHONY: m0-session
m0-session: ## M0 第 1-2 步：握手 + 建会话（不需要 KVM，不需要 API key）
	go run ./cmd/acp-smoke -agent "$(AGENT)" -cwd $(CWD) -skip-prompt

.PHONY: m0
m0: ## M0 第 1-4 步：本地直连 agent 跑通全链路
	go run ./cmd/acp-smoke -agent "$(AGENT)" -cwd $(CWD)

.PHONY: proxy
proxy: ## 起 acp-proxy（沙箱内的角色），监听 :8080
	go run ./cmd/acp-proxy -listen :8080 -cwd $(CWD) -agent "$(AGENT)"

.PHONY: m0-ws
m0-ws: ## M0 第 5 步：通过 acp-proxy 的 WSS 跑 ACP
	go run ./cmd/acp-smoke -url $(PROXY_URL) -skip-prompt

.PHONY: m0-ws-resume
m0-ws-resume: ## 验证跨进程恢复：SESSION=<id> make m0-ws-resume
	@test -n "$(SESSION)" || (echo "用法：SESSION=<sessionId> make m0-ws-resume" && exit 1)
	go run ./cmd/acp-smoke -url $(PROXY_URL) -session $(SESSION) -resume-only

# ── 本地沙箱（开发调试主线）─────────────────────────────────
#
# 目标：在 Mac 上跑通 worker → acp-proxy → agent 的完整链路，不需要 x86_64 + KVM。
# 默认 provider=local（宿主机进程 + sandbox-exec 写限制 + APFS 快照）。

SBX      ?= $(BIN)/astra-sandbox
SANDBOX  ?= dev
PROVIDER ?= local

.PHONY: doctor
doctor: build ## 检查当前 provider 缺什么（先跑这个）
	@$(SBX) -provider $(PROVIDER) doctor

.PHONY: dev
dev: build ## 起本地沙箱（幂等，可反复跑）；覆盖 agent 用 make dev AGENT="..."
	@# 只在用户显式 `make dev AGENT=...` 时才把 -agent 传下去。
	@# 不能用 $(if $(AGENT),...) —— AGENT 有文件内默认值（给 m0 用的），那样永远成立，
	@# 结果 CLI 的自动解析（~/.astra/agent-dsh）被一个错误的默认值顶掉。
	@$(SBX) -provider $(PROVIDER) up $(SANDBOX) \
	  $(if $(filter command line,$(origin AGENT)),-agent "$(AGENT)",)

.PHONY: dev-ls
dev-ls: build ## 列出沙箱
	@$(SBX) -provider $(PROVIDER) ls

.PHONY: dev-smoke
dev-smoke: build ## 用 acp-smoke 连当前沙箱，验证 ACP 全链路
	@URL=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) url) && \
	 TOK=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) token) && \
	 echo "→ $$URL" && \
	 $(BIN)/acp-smoke -url "$$URL" -token "$$TOK" -skip-prompt

.PHONY: dev-resume
dev-resume: build ## 验证沙箱重建后旧会话能恢复：make dev-resume SESSION=<id>
	@test -n "$(SESSION)" || (echo "用法：make dev-resume SESSION=<sessionId>" && exit 1)
	@URL=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) url) && \
	 TOK=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) token) && \
	 $(BIN)/acp-smoke -url "$$URL" -token "$$TOK" -session "$(SESSION)" -resume-only

.PHONY: dev-cycle
dev-cycle: build ## 一键验证「暂停 → 重启 → 旧会话恢复」整条链
	@set -e; \
	 URL=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) url); \
	 TOK=$$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) token); \
	 SID=$$($(BIN)/acp-smoke -url "$$URL" -token "$$TOK" -skip-prompt 2>&1 \
	       | sed -n 's/.*sessionId = \([0-9a-f-]*\).*/\1/p' | head -1); \
	 echo "会话 $$SID"; \
	 $(SBX) -provider $(PROVIDER) pause $(SANDBOX); \
	 $(SBX) -provider $(PROVIDER) up $(SANDBOX); \
	 echo "新端点 $$($(SBX) -provider $(PROVIDER) endpoint $(SANDBOX) url)"; \
	 $(BIN)/acp-smoke -url "$$URL" -token "$$TOK" -session "$$SID" -resume-only

.PHONY: dev-rm
dev-rm: build ## 销毁沙箱
	@$(SBX) -provider $(PROVIDER) rm $(SANDBOX)

# ── M1 控制面（本地跑通全链路）─────────────────────────────
#
# 默认 agent=shell：prompt 当成 shell 命令在沙箱里真执行。
# 不需要任何模型凭据，控制面 / 事件流 / SSE / 审批全都能验证。
# 要换成真 agent：make serve AGENT=dsh

ADDR    ?= 127.0.0.1:7788
DATA    ?= $(HOME)/.astra/data
AGENT_M ?= shell
# 默认走人工审批：本地演示时能真的看到「agent 停下来等你点允许」。
# 不想被打断就 POLICY=workspace-write make serve。
POLICY  ?= ask

.PHONY: serve
# ARGS 原样追加到 astra-server 的命令行末尾。
# 存在的意义：开关类参数（-llm-gateway / -sandbox-quota / -require-auth=false）
# 不该各写一个 make 目标，否则组合一多就爆炸。
#   用法：make serve ARGS="-llm-gateway -llm-key sk-... -llm-public-url http://127.0.0.1:7788"
serve: build ## 起控制面（浏览器打开 http://127.0.0.1:7788）
	@$(BIN)/astra-server -addr $(ADDR) -data $(DATA) \
	  -provider $(PROVIDER) -agent $(AGENT_M) -policy $(POLICY) $(ARGS)

.PHONY: serve-dsh
serve-dsh: build ## 起控制面，agent 换成 dsh（需要 DEEPSEEK_API_KEY）
	@# 用独立的 data 目录：会话的沙箱 meta 里记着 agent 命令，
	@# shell 会话和 dsh 会话混在一个目录里，重启时会去拉起错误的 agent。
	@test -n "$(DEEPSEEK_API_KEY)" || { echo "缺少 DEEPSEEK_API_KEY：先 export 再用 make serve-dsh"; exit 1; }
	@$(BIN)/astra-server -addr $(ADDR) -data $(HOME)/.astra/data-dsh \
	  -provider $(PROVIDER) -agent dsh -policy $(POLICY) $(ARGS)

# ── 上游源码同步 ──────────────────────────────────────────────

.PHONY: sync
sync: ## 按锁定表拉取所有上游源码
	bash scripts/sync-upstream.sh fetch

.PHONY: sync-check
sync-check: ## 检查上游是否有新提交（只读）
	bash scripts/sync-upstream.sh check

.PHONY: sync-verify
sync-verify: ## 校验本地源码与锁定表是否一致
	bash scripts/sync-upstream.sh verify

# ── 清理 ─────────────────────────────────────────────────────

.PHONY: clean
clean: ## 删除构建产物
	rm -rf $(BIN)

# ── 打包与发布 ────────────────────────────────────────────────
#
# VERSION 有缺省值：app 脚本内默认 0.2.0，release 脚本取 git describe。
# 用法：make release VERSION=v0.3.0

.PHONY: app
app: ## 打包 macOS App + dmg 到 dist/（VERSION=x.y.z make app 可指定版本）
	VERSION="$(VERSION)" ./scripts/build-app.sh

.PHONY: release
release: ## 构建跨平台服务器发布包到 dist/release/（4 平台 + SHA256SUMS）
	VERSION="$(VERSION)" bash scripts/build-release.sh

.PHONY: check
check: ## 质量门：gofmt 检查 + go vet + go test + 前端测试（一条跑完）
	@files=$$(gofmt -l $$(find . -name '*.go' -not -path './third_party/*' -not -path './vendor/*')); \
	if [ -n "$$files" ]; then echo "以下文件未 gofmt："; echo "$$files"; exit 1; fi
	go vet ./cmd/... ./internal/...
	go test ./cmd/... ./internal/... -count=1 -timeout 120s
	cd $(WEB) && $(NPM) test
