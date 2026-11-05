#!/usr/bin/env bash
#
# 上游源码同步工具
#
#   bash scripts/sync-upstream.sh check              只报告哪些上游有新提交
#   bash scripts/sync-upstream.sh fetch              按 lock 里的 commit 重放源码
#   bash scripts/sync-upstream.sh update <name>      升级某个上游到最新，并改写 lock
#   bash scripts/sync-upstream.sh verify             校验本地内容与 lock 是否一致
#
# 所有上游信息来自 third_party/upstream.lock.toml，本脚本不含任何硬编码的仓库地址或版本。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOCK="$ROOT/third_party/upstream.lock.toml"

# 代理：优先用 ASTRA_PROXY，否则沿用仓库的 git http.proxy（本机经过 Clash）
PROXY="${ASTRA_PROXY:-$(git -C "$ROOT" config --get http.proxy 2>/dev/null || true)}"
CURL_OPTS=(-sSL --max-time 180)
# git config 里的代理可能是很久以前设的（Clash 关掉后那个端口就没人监听）。
# 不探测的话每个请求都要先撞一次连接失败，5 个文件就是 5 次超时等待——
# 看起来像「下载挂了」，其实只是代理死了。探一次，不通就走直连。
if [ -n "$PROXY" ]; then
  if curl -s -m 3 -o /dev/null -x "$PROXY" https://api.github.com/zen 2>/dev/null; then
    CURL_OPTS+=(-x "$PROXY")
  else
    printf '  (代理 %s 连不上，本次走直连)\n' "$PROXY" >&2
    # 关键：并行 worker 与 tarball 分支都从 PROXY / ASTRA_PROXY_EFF 取值，
    # 只把 CURL_OPTS 里的 -x 去掉是不够的——它们会自己再加回来。
    PROXY=""
  fi
fi

# 匿名 API 限额 60 次/小时，批量拉取时建议给个 token（只需 public_repo 读权限）
API_HDRS=(-H 'Accept: application/vnd.github.raw')
if [ -n "${ASTRA_GITHUB_TOKEN:-}" ]; then
  API_HDRS+=(-H "Authorization: Bearer $ASTRA_GITHUB_TOKEN")
fi

# 为什么不直接用 raw.githubusercontent.com：
#   实测本机该域名直连与走代理都返回 000（不可用），
#   而 api.github.com 稳定 200。用 contents API + Accept: raw 拿到的是同样的原始字节。

die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
ok()   { printf '\033[32m✓ %s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

[ -f "$LOCK" ] || die "找不到锁定表：$LOCK"

# ── 读 lock ────────────────────────────────────────────────────
# 输出 TSV：name  mode  repo  ref  commit  dest
list_sources() {
  python3 - "$LOCK" <<'PY'
import sys, tomllib
with open(sys.argv[1], 'rb') as f:
    d = tomllib.load(f)
for s in d.get('source', []):
    print('\t'.join(str(s.get(k, '')) for k in ('name','mode','repo','ref','commit','dest')))
PY
}

# 取某个 source 的 paths 列表（仅 sparse 模式）
source_paths() {
  python3 - "$LOCK" "$1" <<'PY'
import sys, tomllib
with open(sys.argv[1], 'rb') as f:
    d = tomllib.load(f)
for s in d.get('source', []):
    if s.get('name') == sys.argv[2]:
        for p in s.get('paths', []):
            print(p)
PY
}

# github.com/owner/repo(.git) → owner/repo
# 注意：macOS 是 BSD sed，ERE 不支持惰性量词 +?，所以用 python 代劳
slugify() {
  python3 -c '
import re, sys
m = re.match(r"^.*github\.com[:/]([^/]+/[^/]+?)(\.git)?/?$", sys.argv[1].strip())
print(m.group(1) if m else "")
' "$1"
}

# 查上游某个 ref 的最新 commit
remote_sha() {
  local repo="$1" ref="$2" slug body
  slug="$(slugify "$repo")"
  body="$(curl "${CURL_OPTS[@]}" -H 'Accept: application/vnd.github+json' \
          "https://api.github.com/repos/$slug/commits/$ref" 2>/dev/null || true)"
  printf '%s' "$body" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
    sha=d.get('sha')
    print(sha if sha else '')
except Exception:
    print('')
"
}

# ── check ──────────────────────────────────────────────────────
cmd_check() {
  head_ "检查上游更新（只读，不改任何文件）"
  local name mode repo ref commit dest latest state
  while IFS="$(printf '\t')" read -r name mode repo ref commit dest; do
    [ -z "$name" ] && continue
    latest="$(remote_sha "$repo" "$ref")"
    if [ -z "$latest" ]; then
      printf '  %-18s %-10s \033[33m查询失败\033[0m\n' "$name" "$mode"
      continue
    fi
    if [ "$latest" = "$commit" ]; then
      state="$(printf '\033[32m已最新\033[0m')"
    else
      state="$(printf '\033[33m有新提交 %s → %s\033[0m' "${commit:0:10}" "${latest:0:10}")"
    fi
    printf '  %-18s %-10s %s\n' "$name" "$mode" "$state"
  done < <(list_sources)
  printf '\n  升级用：bash scripts/sync-upstream.sh update <name>\n'
  printf '  改完 lock 后务必人工 review diff，尤其 deepseek-harness（ACP）和 cubesandbox（网络/存储）\n'
}

# ── fetch: submodule ───────────────────────────────────────────
fetch_submodule() {
  local name="$1" repo="$2" commit="$3" dest="$4"
  local abs="$ROOT/$dest"

  # 已经检出到目标 commit 就跳过网络。理由：上游整仓 351MB，
  # `submodule update` 每次都会去连远端，即使本地已经就绪——
  # 没网的机器上 `make sync` 会被这一下卡住，而它其实什么都不用做。
  if [ -d "$abs" ] && [ "$(git -C "$abs" rev-parse HEAD 2>/dev/null)" = "$commit" ]; then
    info "$name: 已就绪 @ ${commit:0:12}，跳过网络"
  else
    if git -C "$ROOT" config --file .gitmodules --get-regexp "path" 2>/dev/null | grep -qx "submodule\..*\.path $dest"; then
      info "$name: submodule 已登记，按 commit 检出"
      git -C "$ROOT" submodule update --init --depth 1 -- "$dest" 2>/dev/null || true
    elif [ -d "$abs/.git" ] || [ -f "$abs/.git" ]; then
      info "$name: 目录已存在且是 git 仓库，跳过 add"
    else
      info "$name: 添加 submodule $repo"
      git -C "$ROOT" submodule add --depth 1 "$repo" "$dest" 2>&1 | sed 's/^/    /'
    fi

    [ -d "$abs" ] || die "$name: submodule 目录不存在：$abs"
    info "$name: 检出 ${commit:0:12}"
    git -C "$abs" fetch --depth 1 origin "$commit" 2>/dev/null || true
    git -C "$abs" checkout -q "$commit" 2>/dev/null || {
      git -C "$abs" fetch origin 2>/dev/null || true
      git -C "$abs" checkout -q "$commit"
    }
  fi

  # 稀疏检出：submodule 也能「只把需要的子树放进工作区」。
  # 关键区别——不是删上游文件：
  #   · 删文件会让 submodule 变脏（git status 有改动），GitHub 上看到的也不再是干净上游
  #   · sparse-checkout 只写本地配置，不动 commit，上游完整性不受影响
  # 所以「裁切」在这里是检出范围，不是修改源码。
  local paths
  paths="$(source_paths "$name")"
  if [ -n "$paths" ]; then
    local dirs=()
    while IFS= read -r p; do
      [ -n "$p" ] && dirs+=("${p%/}")
    done <<< "$paths"
    if [ "${#dirs[@]}" -gt 0 ]; then
      info "$name: 稀疏检出 ${#dirs[@]} 项（其余目录不进工作区）"
      git -C "$abs" sparse-checkout init --cone 2>/dev/null || true
      # cone 模式下根目录文件始终包含，正好覆盖 package.json / LICENSE / NOTICE 这类根元数据
      git -C "$abs" sparse-checkout set "${dirs[@]}" 2>&1 | sed 's/^/    /'
    fi
  fi

  ok "$name @ $(git -C "$abs" rev-parse --short HEAD)"
}

# ── fetch: tarball ─────────────────────────────────────────────
# 整仓快照，不含 git 历史。用于「git 协议拉不动、但需要完整源码可读」的场景。
#
# 注意：codeload 是边生成边推流的，不返回 Content-Length，也不支持 Range 断点续传，
# 所以这里不能设短超时（大仓库几十到上百 MB，本机实测 ~220KB/s 要十几分钟）。
# 用 --max-time 0 不限总时长，靠 --speed-limit/--speed-time 检测「卡死」而不是「慢」。
fetch_tarball() {
  local name="$1" repo="$2" commit="$3" dest="$4"
  local slug abs tmp sha
  slug="$(slugify "$repo")"
  abs="$ROOT/$dest"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN

  info "$name: 下载快照 codeload/$slug@${commit:0:12}"
  info "  （大仓库可能耗时数分钟，只有慢到极致才失败：低于 2KB/s 持续 60s）"
  curl "${CURL_OPTS[@]}" --max-time 0 \
       --speed-limit 2048 --speed-time 60 \
       --retry 3 --retry-delay 5 --retry-all-errors \
       "https://codeload.github.com/$slug/tar.gz/$commit" -o "$tmp/src.tgz" \
    || die "$name: 快照下载失败"
  local bytes; bytes="$(wc -c < "$tmp/src.tgz" | tr -d ' ')"
  [ "$bytes" -gt 1024 ] || die "$name: 下载内容异常（${bytes} bytes）"
  info "  $((bytes / 1024 / 1024)) MB"

  sha="$(shasum -a 256 "$tmp/src.tgz" | cut -d' ' -f1)"

  rm -rf "$abs"
  mkdir -p "$abs"
  tar -xzf "$tmp/src.tgz" -C "$abs" --strip-components=1 || die "$name: 解压失败"

  # 仓库体积控制：源码不进主库（几十万行 TypeScript 会让 clone 变得难受），
  # 但 SOURCE 元数据必须入库——它是「这个目录对应哪个上游 commit」的唯一凭证。
  cat > "$abs/.gitignore" <<'EOF'
# 由 scripts/sync-upstream.sh 生成：源码不入库，SOURCE 入库
*
!.gitignore
!SOURCE
EOF

  cat > "$abs/SOURCE" <<EOF
# 自动生成，勿手改。升级请改 third_party/upstream.lock.toml 后跑 sync-upstream.sh
repo    = $repo
commit  = $commit
mode    = tarball（整仓快照，不含 git 历史）
license = 见同目录 LICENSE
fetched = $(date -u +%Y-%m-%dT%H:%M:%SZ)
sha256  = $sha
EOF
  ok "$name @ ${commit:0:12}（$(find "$abs" -type f | wc -l | tr -d ' ') 个文件）"
}

# ── fetch: sparse ──────────────────────────────────────────────
# 单个文件：走 contents API（Accept: raw 直接拿原始字节）
fetch_one_file() {  # slug commit path outfile
  local slug="$1" commit="$2" path="$3" out="$4"
  curl "${CURL_OPTS[@]}" "${API_HDRS[@]}" \
       "https://api.github.com/repos/$slug/contents/$path?ref=$commit" -o "$out" \
    || return 1
  # API 失败时会返回 JSON 错误体，这里做个最低限度校验
  if [ ! -s "$out" ]; then
    printf '    \033[31m空文件\033[0m\n'; return 1
  fi
  if head -c 200 "$out" | grep -q '"message"'; then
    printf '    \033[31mAPI 报错：%s\033[0m\n' "$(head -c 160 "$out")"; return 1
  fi
  return 0
}

# 取某个 tree 下某子目录的 tree sha（git trees API 不递归，一次一层）
tree_child_sha() {  # slug parentsha name
  curl "${CURL_OPTS[@]}" -H 'Accept: application/vnd.github+json' \
       ${ASTRA_GITHUB_TOKEN:+-H "Authorization: Bearer $ASTRA_GITHUB_TOKEN"} \
       "https://api.github.com/repos/$1/git/trees/$2" 2>/dev/null \
  | python3 -c "
import json,sys
want=sys.argv[1]
try: d=json.load(sys.stdin)
except Exception: raise SystemExit(1)
for e in d.get('tree',[]):
    if e.get('path')==want and e.get('type')=='tree':
        print(e['sha']); break
" "$3"
}

# 把一个目录展开成「仓库内相对路径」列表。
# 为什么要这个：packages/acp/ 下有 35 个文件，在 lock 里逐个列出来没法维护。
# 走 git trees 而不是 contents 递归：前者一次请求拿到整棵子树，后者每个目录一次。
expand_dir() {  # slug commit dirpath（不带尾斜杠）
  local slug="$1" commit="$2" dir="$3"
  local sha="$commit" part
  local oldifs="$IFS"; IFS='/'
  for part in $dir; do
    sha="$(tree_child_sha "$slug" "$sha" "$part")"
    [ -n "$sha" ] || { IFS="$oldifs"; return 1; }
  done
  IFS="$oldifs"
  curl "${CURL_OPTS[@]}" -H 'Accept: application/vnd.github+json' \
       ${ASTRA_GITHUB_TOKEN:+-H "Authorization: Bearer $ASTRA_GITHUB_TOKEN"} \
       "https://api.github.com/repos/$slug/git/trees/$sha?recursive=1" 2>/dev/null \
  | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: raise SystemExit(1)
for e in d.get('tree',[]):
    if e.get('type')=='blob':
        print(e['path'])
"
}

# 把 lock 里的 paths 展开成实际文件清单（目录项递归展开，文件项原样保留）
resolve_paths() {  # name slug commit
  local name="$1" slug="$2" commit="$3" p files
  while IFS= read -r p; do
    [ -z "$p" ] && continue
    case "$p" in
      */)
        files="$(expand_dir "$slug" "$commit" "${p%/}")" \
          || die "$name: 展开目录失败 $p"
        [ -n "$files" ] || die "$name: 目录为空或不存在 $p"
        printf '%s\n' "$files" | sed "s|^|$p|"
        ;;
      *) printf '%s\n' "$p" ;;
    esac
  done < <(source_paths "$name")
}

# 并行下载的 worker。不能直接复用 fetch_one_file：
# bash 数组没法 export，子 shell 里只能从环境变量重建 curl 参数。
_dl_one() {  # slug commit path abs
  local slug="$1" commit="$2" p="$3" abs="$4"
  local -a opts=(-sSL --max-time 120)
  local -a hdrs=(-H 'Accept: application/vnd.github.raw')
  [ -n "${ASTRA_PROXY_EFF:-}" ] && opts+=(-x "$ASTRA_PROXY_EFF")
  [ -n "${ASTRA_GITHUB_TOKEN:-}" ] && hdrs+=(-H "Authorization: Bearer $ASTRA_GITHUB_TOKEN")

  mkdir -p "$abs/$(dirname "$p")"
  curl "${opts[@]}" "${hdrs[@]}" \
       "https://api.github.com/repos/$slug/contents/$p?ref=$commit" -o "$abs/$p" \
    || return 1
  [ -s "$abs/$p" ] || return 1
  # API 报错时会返回 JSON 错误体
  if head -c 200 "$abs/$p" | grep -q '"message"'; then
    rm -f "$abs/$p"; return 1
  fi
  return 0
}

fetch_sparse() {
  local name="$1" repo="$2" commit="$3" dest="$4"
  local slug abs p list n j
  slug="$(slugify "$repo")"
  abs="$ROOT/$dest"
  mkdir -p "$abs"

  # 只解析一次。目录展开要打 API，解析两次等于白花一倍配额。
  list="$(mktemp)"
  resolve_paths "$name" "$slug" "$commit" > "$list" || die "$name: 解析文件清单失败"
  n="$(wc -l < "$list" | tr -d ' ')"
  [ "$n" -gt 0 ] || die "$name: 文件清单为空"

  j="${ASTRA_FETCH_JOBS:-8}"
  # ⚠️ 变量名紧挨全角字符时必须加花括号：macOS 自带 bash 3.2 不认 UTF-8 标识符，
  #    会把「）」的首字节吞进变量名，报 `j?: unbound variable`。
  info "$name: 从 $slug@${commit:0:12} 拉取 $n 个文件（并发 ${j}）"

  # 300 个小文件串行要 6 分钟，并发后 1 分钟内。xargs 任一子进程非零退出即整体失败。
  export -f _dl_one
  export ASTRA_PROXY_EFF="$PROXY"
  export P_SLUG="$slug" P_COMMIT="$commit" P_ABS="$abs"
  tr '\n' '\0' < "$list" \
    | xargs -0 -P "$j" -n 1 bash -c '_dl_one "$P_SLUG" "$P_COMMIT" "$1" "$P_ABS"' _ \
    || die "$name: 有文件下载失败（看上面哪个路径为空）"
  info "  ✓ 全部落地（$(du -sh "$abs" | cut -f1)）"

  # 附上许可原文，避免「源码在但许可丢了」
  if fetch_one_file "$slug" "$commit" "LICENSE" "$abs/LICENSE" 2>/dev/null; then
    info "  ↓ LICENSE ($(wc -c < "$abs/LICENSE" | tr -d ' ') bytes)"
  else
    printf '（上游根目录未取到 LICENSE 文件，请手工确认许可）\n' > "$abs/LICENSE"
  fi

  cat > "$abs/SOURCE" <<EOF
# 自动生成，勿手改。升级请改 third_party/upstream.lock.toml 后跑 sync-upstream.sh
repo    = $repo
commit  = $commit
mode    = sparse（只取下列范围，不是整仓）
license = 见同目录 LICENSE
fetched = $(date -u +%Y-%m-%dT%H:%M:%SZ)
files   = $n

lock 里声明的范围（目录项已递归展开）：
$(source_paths "$name" | sed 's/^/  - /')
EOF
  rm -f "$list"
  ok "$name @ ${commit:0:12}（$n 个文件）"
}

cmd_fetch() {
  local target="${1:-}"
  if [ -n "$target" ]; then head_ "按锁定表重放：$target"; else head_ "按锁定表重放上游源码"; fi
  local name mode repo ref commit dest
  while IFS="$(printf '\t')" read -r name mode repo ref commit dest; do
    [ -z "$name" ] && continue
    if [ -n "$target" ] && [ "$name" != "$target" ]; then continue; fi
    case "$mode" in
      submodule) fetch_submodule "$name" "$repo" "$commit" "$dest" ;;
      tarball)   fetch_tarball   "$name" "$repo" "$commit" "$dest" ;;
      sparse)    fetch_sparse    "$name" "$repo" "$commit" "$dest" ;;
      *)         info "$name: 跳过（mode=$mode 由 go mod 管理）" ;;
    esac
    printf '\n'
  done < <(list_sources)
  ok "完成"
}

# ── update ─────────────────────────────────────────────────────
cmd_update() {
  local target="${1:-}"
  [ -n "$target" ] || die "用法：bash scripts/sync-upstream.sh update <name>（可用 check 查看名称）"

  head_ "升级上游：$target"
  local name mode repo ref commit dest latest found=0
  while IFS="$(printf '\t')" read -r name mode repo ref commit dest; do
    [ "$name" = "$target" ] || continue
    found=1
    latest="$(remote_sha "$repo" "$ref")"
    [ -n "$latest" ] || die "查询 $repo 失败"
    if [ "$latest" = "$commit" ]; then
      ok "$name 已经是最新（${commit:0:12}），lock 未改动"
      return 0
    fi
    info "旧 commit : $commit"
    info "新 commit : $latest"

    # 改写 lock 里该 source 段的 commit（按 [[source]] 分段，避免误伤别的段）
    python3 - "$LOCK" "$name" "$latest" <<'PY'
import re, sys
path, name, newsha = sys.argv[1], sys.argv[2], sys.argv[3]
txt = open(path, encoding='utf-8').read()
blocks = re.split(r'(?m)^\[\[source\]\]\s*$', txt)
out = [blocks[0]]
hit = False
for b in blocks[1:]:
    if re.search(r'(?m)^\s*name\s*=\s*"%s"\s*$' % re.escape(name), b):
        b2 = re.sub(r'(?m)^(\s*commit\s*=\s*")[0-9a-f]{40}(")', r'\g<1>%s\g<2>' % newsha, b, count=1)
        hit = b2 != b
        b = b2
    out.append(b)
if not hit:
    sys.exit('lock 里没找到 %s 的 commit 行' % name)
new = '[[source]]'.join(out)
new = re.sub(r'(?m)^updated\s*=\s*"[^"]*"', 'updated = "%s"' % __import__('datetime').date.today().isoformat(), new, count=1)
open(path, 'w', encoding='utf-8').write(new)
print('  lock 已更新')
PY

    # 立即按新 commit 拉一遍
    case "$mode" in
      submodule) fetch_submodule "$name" "$repo" "$latest" "$dest" ;;
      tarball)   fetch_tarball   "$name" "$repo" "$latest" "$dest" ;;
      sparse)    fetch_sparse    "$name" "$repo" "$latest" "$dest" ;;
    esac
  done < <(list_sources)

  [ "$found" = 1 ] || die "锁定表里没有名为 $target 的 source"
  printf '\n'
  ok "升级完成。记得 review diff：git diff third_party/ && git status"
}

# ── verify ─────────────────────────────────────────────────────
cmd_verify() {
  head_ "校验本地内容与锁定表是否一致"
  local name mode repo ref commit dest abs bad=0
  while IFS="$(printf '\t')" read -r name mode repo ref commit dest; do
    [ -z "$name" ] && continue
    abs="$ROOT/$dest"
    case "$mode" in
      submodule)
        if [ ! -d "$abs" ]; then
          printf '  %-18s \033[31m缺失（未 fetch）\033[0m\n' "$name"; bad=1; continue
        fi
        local cur; cur="$(git -C "$abs" rev-parse HEAD 2>/dev/null || echo '')"
        if [ "$cur" = "$commit" ]; then
          printf '  %-18s \033[32m一致 %s\033[0m\n' "$name" "${commit:0:12}"
        else
          printf '  %-18s \033[31m不一致：本地 %s / lock %s\033[0m\n' "$name" "${cur:0:12}" "${commit:0:12}"; bad=1
        fi
        ;;
      sparse|tarball)
        if [ ! -f "$abs/SOURCE" ]; then
          printf '  %-18s \033[31m缺失（未 fetch）\033[0m\n' "$name"; bad=1; continue
        fi
        if grep -q "commit  *= *$commit" "$abs/SOURCE" || grep -q "commit = $commit" "$abs/SOURCE"; then
          printf '  %-18s \033[32m一致 %s\033[0m\n' "$name" "${commit:0:12}"
        else
          printf '  %-18s \033[31m不一致\033[0m\n' "$name"; bad=1
        fi
        ;;
      *) printf '  %-18s （mode=%s，由 go mod 管理）\n' "$name" "$mode" ;;
    esac
  done < <(list_sources)
  [ "$bad" = 0 ] && ok "全部一致" || die "存在不一致，跑 fetch 修正"
}

# ── 入口 ───────────────────────────────────────────────────────
case "${1:-}" in
  check)  cmd_check ;;
  fetch)  cmd_fetch "${2:-}" ;;
  update) cmd_update "${2:-}" ;;
  verify) cmd_verify ;;
  *)
    cat <<EOF
上游源码同步工具

  bash scripts/sync-upstream.sh check              只报告哪些上游有新提交
  bash scripts/sync-upstream.sh fetch              按 lock 里的 commit 重放源码
  bash scripts/sync-upstream.sh update <name>      升级某个上游到最新并改写 lock
  bash scripts/sync-upstream.sh verify             校验本地内容与 lock 是否一致

锁定表：third_party/upstream.lock.toml
EOF
    exit 1
    ;;
esac
