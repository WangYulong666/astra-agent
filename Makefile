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

# ── 依赖组件（Milvus / Langfuse）──────────────────────────────
#
# 这两套是**可选能力**，刻意不并进 make serve 的启动路径：
# 起不来的时候平台照常可用，只是记忆 / 观测关闭（/v1/meta 里能看出来）。
# 把它们塞进 `make serve` 的依赖里，等于让「没装 docker」这件事阻塞整个开发流。
#
# 数据落在 deploy/<模块>/data/（已 gitignore，见 deploy/README.md）：
#   deps-down  保留数据（容器没了，collection 与 trace 还在）
#   deps-clean 才删数据（要二次确认）

COMPOSE     ?= docker compose
VECTOR_DIR  := deploy/vector
OBS_DIR     := deploy/observability
VECTOR_YAML := $(VECTOR_DIR)/compose.yaml
OBS_YAML    := $(OBS_DIR)/compose.yaml

.PHONY: vector-up
vector-up: ## 起 Milvus 全家桶（etcd + SeaweedFS + milvus）
	@$(COMPOSE) -f $(VECTOR_YAML) up -d
	@echo "→ milvus 首次启动要 60-90s 才 ready；用 make deps-status 看进度"

.PHONY: vector-down
vector-down: ## 停 Milvus 全家桶（保留 deploy/vector/data）
	@$(COMPOSE) -f $(VECTOR_YAML) down 2>/dev/null || true
	@echo "→ 容器已停，deploy/vector/data 保留"

.PHONY: obs-up
obs-up: ## 起 Langfuse 全家桶（pg + clickhouse + valkey + SeaweedFS + web/worker）
	@$(COMPOSE) -f $(OBS_YAML) up -d
	@echo "→ langfuse-web 首启要跑两次迁移，1-2 分钟；用 make deps-status 看进度"

.PHONY: obs-down
obs-down: ## 停 Langfuse 全家桶（保留 deploy/observability/data）
	@$(COMPOSE) -f $(OBS_YAML) down 2>/dev/null || true
	@echo "→ 容器已停，deploy/observability/data 保留"

.PHONY: deps-up
deps-up: vector-up obs-up ## 起全部依赖（向量 + 观测）
	@printf '\n→ 30-120s 后跑 make deps-status 确认；首次拉镜像会更久\n'

.PHONY: deps-down
deps-down: ## 停全部依赖容器（保留数据）
	@$(COMPOSE) -f $(VECTOR_YAML) down 2>/dev/null || true
	@$(COMPOSE) -f $(OBS_YAML) down 2>/dev/null || true
	@echo "→ 容器已停，数据还在 deploy/*/data/；连数据一起删用 make deps-clean"

.PHONY: deps-clean
deps-clean: ## 停全部依赖并删除数据（二次确认；FORCE=1 跳过）
	@printf '\033[33m⚠️  即将删除：\033[0m\n'
	@printf '    %s/data/   %s/data/\n' '$(VECTOR_DIR)' '$(OBS_DIR)'
	@printf '    含 Milvus collection、Langfuse 的 trace 与登录账号 —— 删了不可恢复。\n'
	@if [ "$(FORCE)" = "1" ]; then echo "FORCE=1，跳过确认"; \
	elif [ ! -t 0 ]; then echo "非交互环境，未确认，已取消（要跳过确认用 make deps-clean FORCE=1）"; exit 1; \
	else \
	  printf '确认请输入 yes：'; read confirm; \
	  [ "$$confirm" = "yes" ] || { echo "已取消"; exit 1; }; \
	fi
	@$(COMPOSE) -f $(VECTOR_YAML) down --volumes --remove-orphans 2>/dev/null || true
	@$(COMPOSE) -f $(OBS_YAML) down --volumes --remove-orphans 2>/dev/null || true
	@rm -rf $(VECTOR_DIR)/data $(OBS_DIR)/data
	@echo "→ 已清空"

.PHONY: deps-status
deps-status: ## 逐个探测依赖的端口/健康端点，输出「谁没起来」
	@miss=0; total=0; \
	printf '\n\033[1m依赖组件状态\033[0m（探本机回环端口，不看 docker 状态）\n'; \
	if command -v docker >/dev/null 2>&1; then printf '  docker: 可用\n'; \
	else printf '  docker: \033[33m未安装\033[0m —— 全部显示「未起来」是预期结果，不是配置错\n'; fi; \
	printf '\n'; \
	port_open() { (echo > /dev/tcp/$${1%:*}/$${1##*:}) >/dev/null 2>&1; }; \
	http_ok()   { curl -sf -m 3 -o /dev/null "$$1"; }; \
	check() { \
	  total=$$((total+1)); \
	  if ! port_open "$$2"; then \
	    miss=$$((miss+1)); \
	    printf '  \033[31m✗\033[0m %-14s %s 未起来：%s 端口无响应，先看 %s\n' "$$1" "$$1" "$$2" "$$4"; \
	  elif [ -n "$$3" ]; then \
	    if http_ok "$$3"; then \
	      printf '  \033[32m✓\033[0m %-14s %s 200\n' "$$1" "$$3"; \
	    else \
	      miss=$$((miss+1)); \
	      printf '  \033[33m!\033[0m %-14s %s 端口通了但健康端点没绿（%s），再等 30s；仍不行看 %s\n' "$$1" "$$1" "$$3" "$$4"; \
	    fi; \
	  else \
	    printf '  \033[32m✓\033[0m %-14s %s 可连（该组件没有 HTTP 健康端点）\n' "$$1" "$$2"; \
	  fi; \
	}; \
	printf '\033[36m— vector：etcd + SeaweedFS + Milvus —\033[0m\n'; \
	check etcd       127.0.0.1:2379 http://127.0.0.1:2379/health '$(COMPOSE) -f $(VECTOR_YAML) logs etcd'; \
	check SeaweedFS  127.0.0.1:9333 http://127.0.0.1:9333/cluster/status '$(COMPOSE) -f $(VECTOR_YAML) logs seaweedfs'; \
	check S3-vector  127.0.0.1:8333 '' '$(COMPOSE) -f $(VECTOR_YAML) logs seaweedfs'; \
	check Milvus     127.0.0.1:19530 http://127.0.0.1:9091/healthz '$(COMPOSE) -f $(VECTOR_YAML) logs milvus'; \
	printf '\n\033[36m— observability：Postgres + ClickHouse + Valkey + SeaweedFS + Langfuse —\033[0m\n'; \
	check Postgres   127.0.0.1:5433 '' '$(COMPOSE) -f $(OBS_YAML) logs postgres'; \
	check ClickHouse 127.0.0.1:8123 http://127.0.0.1:8123/ping '$(COMPOSE) -f $(OBS_YAML) logs clickhouse'; \
	check Valkey     127.0.0.1:6380 '' '$(COMPOSE) -f $(OBS_YAML) logs valkey'; \
	check S3-observe 127.0.0.1:8334 '' '$(COMPOSE) -f $(OBS_YAML) logs seaweedfs'; \
	check Langfuse   127.0.0.1:3000 http://127.0.0.1:3000/api/public/health '$(COMPOSE) -f $(OBS_YAML) logs langfuse-web'; \
	printf '\n'; \
	if [ $$miss -eq 0 ]; then \
	  printf '\033[32m全部 %d 项就绪\033[0m\n' $$total; \
	else \
	  printf '%d/%d 项未就绪。首次启动要等 1-2 分钟（milvus 建 WAL、langfuse 跑迁移）。\n' $$miss $$total; \
	  printf '排障顺序见 deploy/README.md：先基础依赖（etcd/S3 → milvus / clickhouse）再应用层。\n'; \
	fi; \
	exit 0
