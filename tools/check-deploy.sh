#!/usr/bin/env bash
# deploy/ 静态自检
#
# 为什么要有这个脚本：本机（以及 CI 的大部分步骤里）没有 kubectl / kustomize /
# 一个真集群，`kubectl apply --dry-run` 那种「官方校验」跑不了。于是退一步，
# 把「不依赖集群就能查出来的错误」全部前置到这里：
#   - 占位符漏网（apply 阶段才炸）
#   - YAML 语法错（apply 阶段才炸）
#   - 工作负载缺探针/资源限制（部署成功但线上会出事）
#   - 引用了不存在的 ConfigMap/Secret（pod 起来才 CreateContainerConfigError）
#   - kustomization 列了不存在的文件（kustomize build 直接失败）
#   - 硬编码密码（最贵的一类错误，进了 git 就收不回来）
#   - args 里写了 main.go 里不存在的 flag（进程启动就 exit 2）
#   - 迁移用的 DDL 副本与 Go 源码里的不一致（建出来的表和代码期望的不一样）
#
# 它**不能**证明的东西（别拿它当部署成功的证据）：
#   - 集群能不能调度、镜像能不能拉、探针在真依赖下会不会绿
#   - RBAC / PodSecurity / StorageClass / CNI 是否就绪
# 这些只能靠真集群，见 README「未经真实集群验证」一节。
#
# 用法：bash tools/check-deploy.sh        （失败退出码非 0）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec python3 - "$ROOT" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys

ROOT = sys.argv[1]
DEPLOY = os.path.join(ROOT, "deploy")

OK, BAD, WARN = "\033[32m✅\033[0m", "\033[31m❌\033[0m", "\033[33m⚠️\033[0m"
failures = []
warnings = []


def section(title):
    print("\n\033[1;36m▸ %s\033[0m" % title)


def ok(msg):
    print("  %s %s" % (OK, msg))


def bad(msg):
    print("  %s %s" % (BAD, msg))
    failures.append(msg)


def warn(msg):
    print("  %s %s" % (WARN, msg))
    warnings.append(msg)


# ── YAML 解析器探测 ───────────────────────────────────────────
# 本机实测 python3 没有 pyyaml（macOS 自带 python 不带第三方包），
# 所以第二选择是 ruby 的 psych（macOS 与多数发行版自带）：
# 用 ruby 把 YAML 转成 JSON，再交给下面同一套 Python 逻辑处理，
# 避免「两套校验逻辑各写一遍、慢慢跑偏」。
RUBY_TO_JSON = (
    "require 'yaml'; require 'json'; "
    "puts JSON.generate(YAML.load_stream(STDIN.read).compact)"
)

MODE = "fallback"
try:
    import yaml  # noqa: F401
    MODE = "python"
except Exception:
    if shutil.which("ruby"):
        probe = subprocess.run(
            ["ruby", "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.load_stream(STDIN.read).compact)"],
            input=b"a: 1\n", capture_output=True)
        if probe.returncode == 0:
            MODE = "ruby"


def load_docs(path):
    """返回该文件里的所有 YAML 文档（跳过空文档）。解析失败抛异常。"""
    if MODE == "python":
        import yaml as pyyaml
        with open(path, "rb") as f:
            return [d for d in pyyaml.safe_load_all(f) if d is not None]
    if MODE == "ruby":
        with open(path, "rb") as f:
            p = subprocess.run(["ruby", "-ryaml", "-rjson", "-e", RUBY_TO_JSON],
                               stdin=f, capture_output=True)
        if p.returncode != 0:
            raise RuntimeError(p.stderr.decode("utf-8", "replace").strip()[:500])
        return [d for d in json.loads(p.stdout.decode("utf-8") or "[]") if d is not None]
    raise RuntimeError("没有可用的 YAML 解析器")


# future/ 是「架构前瞻但代码未实现」的组件，不参与 apply。
# 它们仍要做 YAML 语法检查（第 2 项），但不应进入工作负载结构与指标端口的
# 断言 —— 否则会逼着人给一个永远不会被调度的 Deployment 补齐探针，
# 反过来掩盖「它其实还没实现」这个事实。
FUTURE_DIR = os.path.join(DEPLOY, "k8s", "future")


def applies(p):
    return not p.startswith(FUTURE_DIR + os.sep)


def yaml_files():
    out = []
    for dirpath, _, names in os.walk(DEPLOY):
        for n in names:
            if n.endswith((".yaml", ".yml")):
                out.append(os.path.join(dirpath, n))
    return sorted(out)


def text_files():
    out = []
    for dirpath, _, names in os.walk(DEPLOY):
        for n in names:
            p = os.path.join(dirpath, n)
            try:
                with open(p, "rb") as f:
                    f.read(4096)
                out.append(p)
            except OSError:
                pass
    return sorted(out)


def rel(p):
    return os.path.relpath(p, ROOT)


print("\033[1mastra deploy 静态自检\033[0m")
print("  根目录: %s" % ROOT)
print("  YAML 解析器: %s" % {"python": "pyyaml", "ruby": "ruby/psych", "fallback": "无（降级为文本断言）"}[MODE])

# ── 1. 占位符 ─────────────────────────────────────────────────
section("1. 禁止占位符（镜像 / tag / 任何 PLACE 开头的假值）")
hits = []
for p in text_files():
    try:
        body = open(p, encoding="utf-8", errors="replace").read()
    except OSError:
        continue
    for i, line in enumerate(body.splitlines(), 1):
        if "PLACEHOLDER" in line.upper() and "PLACEHOLDER" in line:
            hits.append("%s:%d: %s" % (rel(p), i, line.strip()[:90]))
if hits:
    bad("发现 %d 处占位符（apply 时会变成非法镜像引用）" % len(hits))
    for h in hits[:10]:
        print("       " + h)
else:
    ok("没有占位符")

# ── 2. YAML 可解析 ────────────────────────────────────────────
section("2. 每个 YAML 都能被解析")
docs_by_file = {}
parse_failed = []
for p in yaml_files():
    try:
        docs_by_file[p] = load_docs(p)
        ok("%s（%d 个文档）" % (rel(p), len(docs_by_file[p])))
    except Exception as e:  # noqa: BLE001
        parse_failed.append(p)
        bad("%s 解析失败: %s" % (rel(p), e))
if MODE == "fallback" and not parse_failed:
    warn("没有 YAML 解析器，第 2 项以后的结构断言已降级为 grep 文本断言，可信度低于正常模式")

objects = []  # (path, obj)
for p, docs in docs_by_file.items():
    if not applies(p):
        continue
    for d in docs:
        if isinstance(d, dict) and d.get("kind"):
            objects.append((p, d))

workload_kinds = ("Deployment", "StatefulSet", "DaemonSet")
workloads = [(p, o) for p, o in objects if o.get("kind") in workload_kinds]

# ── 3. 工作负载结构 ──────────────────────────────────────────
section("3. 工作负载必须带 image / livenessProbe / readinessProbe / resources.limits")


def containers_of(obj):
    spec = (obj.get("spec") or {}).get("template", {}).get("spec", {}) or {}
    return spec.get("containers") or []


if not workloads:
    bad("一个 Deployment/StatefulSet 都没找到（路径找错了？）")
for p, o in workloads:
    name = o.get("metadata", {}).get("name", "<无名>")
    problems = []
    cs = containers_of(o)
    if not cs:
        problems.append("没有 containers")
    for c in cs:
        cn = c.get("name", "?")
        img = c.get("image", "")
        if not img:
            problems.append("%s 缺 image" % cn)
        elif not re.match(r"^[a-z0-9][a-z0-9._/:-]*:[A-Za-z0-9._-]+$", img.split("@")[0]) and "@" not in img:
            problems.append("%s 的 image 不是合法引用: %s" % (cn, img))
        if not c.get("livenessProbe"):
            problems.append("%s 缺 livenessProbe" % cn)
        if not c.get("readinessProbe"):
            problems.append("%s 缺 readinessProbe" % cn)
        lim = ((c.get("resources") or {}).get("limits") or {})
        if not lim:
            problems.append("%s 缺 resources.limits" % cn)
        elif "memory" not in lim:
            problems.append("%s 的 limits 没设 memory（OOM 时会拖垮整节点）" % cn)
    if problems:
        bad("%s/%s: %s" % (o.get("kind"), name, "；".join(problems)))
    else:
        ok("%s/%s 结构完整" % (o.get("kind"), name))

# ── 4. 探针语义 ──────────────────────────────────────────────
section("4. 探针路径：liveness→/healthz，readiness→/readyz")
probe_problems = 0
for p, o in workloads:
    name = o.get("metadata", {}).get("name")
    for c in containers_of(o):
        for probe, want in (("livenessProbe", "/healthz"), ("readinessProbe", "/readyz")):
            pr = c.get(probe) or {}
            http = pr.get("httpGet")
            if not http:
                continue  # exec/tcp 探针（如 PG）不在此列
            got = http.get("path")
            if got != want:
                bad("%s/%s 的 %s 路径是 %s，应为 %s" % (name, c.get("name"), probe, got, want))
                probe_problems += 1
        # 探针端口必须是容器声明过的端口名或号，写错只会表现为「探针永远失败」
        declared = {pt.get("name") for pt in (c.get("ports") or []) if pt.get("name")}
        declared |= {str(pt.get("containerPort")) for pt in (c.get("ports") or [])}
        for probe in ("livenessProbe", "readinessProbe"):
            http = (c.get(probe) or {}).get("httpGet") or {}
            port = http.get("port")
            if port is not None and str(port) not in declared and port not in declared:
                bad("%s/%s 的 %s 指向未声明的端口 %s" % (name, c.get("name"), probe, port))
                probe_problems += 1
if probe_problems == 0:
    ok("探针路径与端口引用一致")

# ── 5. ConfigMap / Secret 引用完整性 ─────────────────────────
section("5. 工作负载引用的 ConfigMap / Secret 必须存在")
cm_names, secret_names, example_secrets = set(), set(), set()
for p, o in objects:
    kind = o.get("kind")
    n = o.get("metadata", {}).get("name")
    if kind == "ConfigMap":
        cm_names.add(n)
    elif kind == "Secret":
        secret_names.add(n)
        if p.endswith("secret.example.yaml"):
            example_secrets.add(n)
    elif kind == "Kustomization":
        for g in (o.get("configMapGenerator") or []):
            if g.get("name"):
                cm_names.add(g["name"])

missing = 0
example_users = {}  # secret 名 -> 引用它的工作负载集合
for p, o in workloads:
    name = o.get("metadata", {}).get("name")
    for c in containers_of(o):
        blob = json.dumps(c)
        for m in re.finditer(r'"configMapKeyRef"\s*:\s*\{[^}]*"name"\s*:\s*"([^"]+)"', blob):
            if m.group(1) not in cm_names:
                bad("%s 引用了不存在的 ConfigMap %s" % (name, m.group(1)))
                missing += 1
        for m in re.finditer(r'"secretKeyRef"\s*:\s*\{[^}]*"name"\s*:\s*"([^"]+)"', blob):
            ref = m.group(1)
            if ref not in secret_names:
                bad("%s 引用了不存在的 Secret %s" % (name, ref))
                missing += 1
            elif ref in example_secrets:
                example_users.setdefault(ref, set()).add(name)
    # envFrom 也要查，否则「整份 ConfigMap 注入」这种写法会被漏掉
    spec = (o.get("spec") or {}).get("template", {}).get("spec", {}) or {}
    for c in spec.get("containers") or []:
        for ef in c.get("envFrom") or []:
            ref = (ef.get("configMapRef") or {}).get("name")
            if ref and ref not in cm_names:
                bad("%s 的 envFrom 引用了不存在的 ConfigMap %s" % (name, ref))
                missing += 1
    for v in spec.get("volumes") or []:
        ref = (v.get("configMap") or {}).get("name")
        if ref and ref not in cm_names:
            bad("%s 的 volume 引用了不存在的 ConfigMap %s" % (name, ref))
            missing += 1
        ref = (v.get("secret") or {}).get("secretName")
        if ref and ref not in secret_names:
            bad("%s 的 volume 引用了不存在的 Secret %s" % (name, ref))
            missing += 1
if missing == 0:
    ok("所有 ConfigMap/Secret 引用都能解析")
for ref in sorted(example_users):
    warn("Secret %s 只在 secret.example.yaml 里有定义，被 %s 引用 —— 真值必须在 apply 前用 kubectl create secret / ESO 注入"
         % (ref, ", ".join(sorted(example_users[ref]))))
print("     ConfigMap: %s" % ", ".join(sorted(x for x in cm_names if x)))
print("     Secret   : %s（其中 example-only: %s）"
      % (", ".join(sorted(x for x in secret_names if x)) or "无",
         ", ".join(sorted(example_secrets)) or "无"))

# ── 6. kustomization 的 resources 都要存在 ───────────────────
section("6. kustomization 引用的文件/目录都存在")
ks_count = 0
stale_latest = set()
for p, o in objects:
    if o.get("kind") != "Kustomization":
        continue
    ks_count += 1
    base = os.path.dirname(p)
    for entry in (o.get("resources") or []):
        target = os.path.normpath(os.path.join(base, entry))
        if not os.path.exists(target):
            bad("%s 的 resources 里有不存在的路径: %s" % (rel(p), entry))
    for g in (o.get("configMapGenerator") or []):
        for f in (g.get("files") or []):
            fp = f.split("=")[-1]
            target = os.path.normpath(os.path.join(base, fp))
            if not os.path.exists(target):
                bad("%s 的 configMapGenerator 引用了不存在的文件: %s" % (rel(p), f))
    for img in (o.get("images") or []):
        if "newTag" in img and img["newTag"] in ("latest", "PLACEHOLDER", ""):
            stale_latest.add(rel(p))
            break
if ks_count == 0:
    bad("没有找到 kustomization.yaml")
else:
    ok("%d 个 kustomization 的引用路径全部存在" % ks_count)
for f in sorted(stale_latest):
    warn("%s 里的 newTag 还是 latest —— 发布前必须由 CI 覆盖成 commit sha（gitops-bump.sh 负责）" % f)

# ── 7. 硬编码密码 ────────────────────────────────────────────
section("7. 不允许硬编码密码/密钥（secret.example.yaml 白名单）")
# 两层：先说结构（env 里的字面值、ConfigMap 里的敏感键、非 example 的 Secret），
# 再用正则兜底那些没被结构化遍历到的地方（比如 helm values）。
# 只做正则是不够的：`{ name: X_PASSWORD, value: "hunter2" }` 这种单行 map 里，
# 键是 name/value，敏感词在 value 上，纯正则会整体漏掉。
SUSPECT_KEY = re.compile(r"(?i)(password|passwd|pwd|secret|token|api[-_]?key)")
PLACEHOLDER_VAL = re.compile(r"(?i)(\$\(|\{\{|replace_me|change_me|changeme|example|<[^>]*>)")
pw_hits = []


def looks_fake(v):
    return isinstance(v, str) and (not v or PLACEHOLDER_VAL.search(v) is not None)


for p, o in objects:
    kind = o.get("kind")
    oname = o.get("metadata", {}).get("name", "?")
    if kind == "ConfigMap":
        for k, v in (o.get("data") or {}).items():
            if SUSPECT_KEY.search(str(k)) and not looks_fake(v):
                pw_hits.append("%s: ConfigMap %s 的键 %s 存了字面值（密钥必须放 Secret）"
                               % (rel(p), oname, k))
    if kind == "Secret" and not p.endswith("secret.example.yaml"):
        for field in ("stringData", "data"):
            for k, v in (o.get(field) or {}).items():
                if not looks_fake(v):
                    pw_hits.append("%s: Secret %s 的 %s 不是明显假值"
                                   % (rel(p), oname, k))
    if kind in workload_kinds:
        for c in containers_of(o):
            for e in (c.get("env") or []):
                n = str(e.get("name", ""))
                if SUSPECT_KEY.search(n) and "value" in e and not looks_fake(e.get("value")):
                    pw_hits.append("%s: %s/%s 的环境变量 %s 写了字面值（应走 valueFrom.secretKeyRef）"
                                   % (rel(p), oname, c.get("name"), n))

PW_PATTERNS = [
    re.compile(r'(?i)\b(password|passwd|pwd)\s*[:=]\s*["\']?([^\s"\'{}]{4,})'),
    re.compile(r'(?i)^\s*[\w.-]*secret[\w.-]*\s*:\s*["\'][^"\']{6,}["\']'),
]
ALLOW_VAL = ("$(", "{{", "${", "valuefrom", "replace_me", "change_me", "changeme", "<", "none", "null")
for p in yaml_files():
    if p.endswith("secret.example.yaml") or not applies(p):
        continue
    for i, line in enumerate(open(p, encoding="utf-8", errors="replace").read().splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue  # 注释里写 password 是正常的技术说明
        for pat in PW_PATTERNS:
            m = pat.search(line)
            if not m:
                continue
            val = (m.group(2) if m.lastindex and m.lastindex >= 2 else "").strip().lower()
            if any(a in val for a in ALLOW_VAL):
                continue
            pw_hits.append("%s:%d: %s" % (rel(p), i, line.strip()[:100]))
            break
if pw_hits:
    bad("发现 %d 处疑似硬编码密钥" % len(pw_hits))
    for h in pw_hits[:10]:
        print("       " + h)
else:
    ok("没有硬编码密码/密钥")

# ── 8. 迁移 DDL 与 Go 源码一致 ───────────────────────────────
section("8. 迁移 Job 用的是应用自己的迁移入口，不是手抄的 DDL")
job = os.path.join(DEPLOY, "k8s/base/migrate-job.yaml")
if not os.path.exists(job):
    bad("缺少 %s" % rel(job))
else:
    body = open(job, encoding="utf-8").read()
    # 为什么查这个：psql 直接跑一份 DDL 副本意味着 DDL 有两个事实来源，
    # 加一个迁移版本就得同步两处，漏一次就变成「Job 成功但表少一张」。
    # -migrate-only 用的是运行时同一份代码里的 schema 与版本表。
    if "-migrate-only" in body:
        ok("migrate-job 使用 astra-server -migrate-only（DDL 单一事实来源）")
    else:
        bad("%s 没有用 -migrate-only —— 手写 DDL 副本迟早与 internal/store/sqlstore 漂移" % rel(job))
    # 反向：确认没有任何地方在引 schema ConfigMap（那说明还留着旧方案）
    leftovers = []
    for root, _dirs, files in os.walk(os.path.join(DEPLOY, "k8s")):
        for f in files:
            if not f.endswith((".yaml", ".yml")):
                continue
            fp = os.path.join(root, f)
            # 只看非注释行：这些文件的注释里会提到旧方案（说明为什么改掉它），
            # 把注释也算作「引用」会让解释历史的注释变成新的失败项。
            for line in open(fp, encoding="utf-8").read().splitlines():
                if line.lstrip().startswith("#"):
                    continue
                if "astra-schema" in line or "schema_pg.sql" in line:
                    leftovers.append(rel(fp))
                    break
    if leftovers:
        bad("仍引用旧 DDL 副本: %s" % ", ".join(leftovers))
    else:
        ok("没有残留的 DDL 副本引用")

# ── 9. args 里的 flag 必须是 main.go 真实存在的 ──────────────
section("9. astra-api 的 args 只用 cmd/astra-server/main.go 里真实存在的 flag")
main_go = os.path.join(ROOT, "cmd/astra-server/main.go")
FLAG_RE = re.compile(r'flag\.(?:String|Bool|Int|Duration|Float64|Var)\(\s*(?:&?\w+\s*,\s*)?"([A-Za-z0-9_-]+)"')
if not os.path.exists(main_go):
    warn("找不到 %s，跳过 flag 检查" % rel(main_go))
else:
    real_flags = set(FLAG_RE.findall(open(main_go, encoding="utf-8").read()))
    if not real_flags:
        warn("从 main.go 里没解析出 flag，正则可能过时了")
    used = set()
    for p, o in workloads:
        if o.get("metadata", {}).get("name") != "astra-api":
            continue
        for c in containers_of(o):
            for a in c.get("args") or []:
                if isinstance(a, str) and a.startswith("-"):
                    used.add(a.lstrip("-").split("=")[0])
    unknown = sorted(used - real_flags)
    if unknown:
        bad("astra-api 的 args 用了 main.go 里不存在的 flag: %s（进程会直接 exit 2）"
            % ", ".join(unknown))
    else:
        ok("args 里的 %d 个 flag 全部存在: %s" % (len(used), ", ".join(sorted(used))))
        never = sorted(real_flags - used)
        if never:
            print("     （main.go 有但清单没用到的 flag: %s）" % ", ".join(never))
    # 反向：清单里出现 ASTRA_ 环境变量，但程序根本不读 —— 只能提示，不能判定
    for p, o in workloads:
        if o.get("metadata", {}).get("name") != "astra-api":
            continue
        for c in containers_of(o):
            names = [e.get("name") for e in (c.get("env") or [])]
            astra = [n for n in names if n and n.startswith("ASTRA_")]
            if astra:
                print("     ASTRA_* 环境变量（多数作 args 的 $(VAR) 载体；闭环相关如 ASTRA_CLI_BIN 由程序直读）: %s"
                      % ", ".join(sorted(astra)))

# ── 10. 指标注解 ─────────────────────────────────────────────
# 只查 astra 自己的服务（harbor.astra.local/astra/*）。第三方组件（postgres 等）
# 没有 /metrics，硬加注解只会产生「抓不到」的假告警。
section("10. astra 自有工作负载带 prometheus 抓取注解，且端口是容器真声明的")
for p, o in workloads:
    name = o.get("metadata", {}).get("name")
    cs = containers_of(o)
    if not any((c.get("image") or "").startswith("harbor.astra.local/") for c in cs):
        print("     %s 不是 astra 自有镜像，跳过指标约定" % name)
        continue
    ann = ((o.get("spec") or {}).get("template", {}).get("metadata", {}) or {}).get("annotations") or {}
    if ann.get("prometheus.io/scrape") != "true":
        bad("%s 缺 prometheus.io/scrape 注解" % name)
        continue
    port = str(ann.get("prometheus.io/port", ""))
    declared = set()
    for c in cs:
        for pt in (c.get("ports") or []):
            if pt.get("containerPort") is not None:
                declared.add(str(pt["containerPort"]))
    if port not in declared:
        bad("%s 的 prometheus.io/port=%s 不是任何容器声明过的 containerPort（抓不到）"
            % (name, port))
    else:
        ok("%s 指标端口 %s 已声明" % (name, port))

# ── 汇总 ─────────────────────────────────────────────────────
print()
if warnings:
    print("%s %d 条提示（不阻塞）" % (WARN, len(warnings)))
if failures:
    print("\033[31m静态自检失败：%d 项\033[0m" % len(failures))
    sys.exit(1)
print("\033[32m静态自检通过\033[0m —— 注意：这只说明清单「自洽」，不代表能部署到集群")
PY
