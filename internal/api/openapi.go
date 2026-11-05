// OpenAPI 契约出口：/v1/openapi.json 与 /docs 浏览页。
//
// 契约正文只有一份，在 internal/api/openapi.yaml：
//   - Go 的 //go:embed 不能引用父目录（".." 是非法的），所以文件必须在包目录下；
//   - 如果同时留一份 docs/openapi.yaml 副本，两份迟早不一致，而「契约漂移」
//     正是本期要消灭的问题。docs/openapi.yaml 因此只放一行跳转说明。
//
// 为什么不用 gopkg.in/yaml.v3：go.mod 里没有它（只有 go.sum 的间接条目），
// 而本期的硬约束是「不许动 go.mod / go.sum」。所以这里自带一个只认本文件
// 所使用子集的解析器（见 parseYAMLSubset 的注释）——不是通用 YAML 实现，
// 但足以把契约转成 JSON，并且解析失败会诚实地回 500。
//
// 为什么启动时解析一次：契约是编译期常量，每个请求都重新解析既慢又
// 把「解析失败」变成「每次请求都 500」。用 sync.Once 缓存结果与错误。
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

//go:embed openapi.yaml
var openapiYAML []byte

var (
	openapiOnce sync.Once
	openapiJSON []byte
	openapiErr  error
)

// openapiDocument 返回契约的 JSON 形态。
//
// 返回 error 而不是「失败时给个空对象」：空对象会让抓取端以为服务正常
// 只是「没有接口」，排查时会白白绕一圈。
func openapiDocument() ([]byte, error) {
	openapiOnce.Do(func() {
		doc, err := parseYAMLSubset(openapiYAML)
		if err != nil {
			openapiErr = err
			return
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			openapiErr = err
			return
		}
		openapiJSON = b
	})
	return openapiJSON, openapiErr
}

func (h *handler) openapiJSON(w http.ResponseWriter, r *http.Request) {
	doc, err := openapiDocument()
	if err != nil {
		h.log.Printf("openapi: 内嵌契约解析失败: %v", err)
		fail(w, http.StatusInternalServerError, "契约解析失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

func (h *handler) docs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsHTML))
}

// ── 极简 YAML 解析 ───────────────────────────────────────────

// parseYAMLSubset 解析 openapi.yaml 用到的 YAML 子集：
//   - 块状 mapping 与 sequence，缩进用空格（2 格一跳）；
//   - 标量：普通值、单引号、双引号；true / false / null 与数字自动转型；
//   - 流式写法只认 [] / [a, b] / {}（不认 {k: v}，那会让解析器复杂度翻倍）；
//   - 注释只能整行，行尾注释不剥。
//
// 为什么不引第三方库：见文件头的说明。为什么不写成通用解析器：越通用越容易
// 在别人的边界上出错，而这份文件的写法完全由本项目控制，配合防漂移测试
// （openapi_test.go 会把每个 path 真请求一遍），解析错了会立刻暴露。
func parseYAMLSubset(src []byte) (any, error) {
	lines, err := scanYAMLLines(src)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, errors.New("契约文件是空的")
	}
	v, next, err := parseYAMLBlock(lines, 0, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if next != len(lines) {
		return nil, fmt.Errorf("第 %d 行缩进异常，多出来的内容无法归属任何层级", lines[next].no)
	}
	return v, nil
}

type yamlLine struct {
	no     int // 源文件行号，报错时指得准
	indent int
	text   string
}

func scanYAMLLines(src []byte) ([]yamlLine, error) {
	raw := strings.Split(string(src), "\n")
	out := make([]yamlLine, 0, len(raw))
	for i, ln := range raw {
		ln = strings.TrimSuffix(ln, "\r")
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := 0
		for indent < len(ln) && ln[indent] == ' ' {
			indent++
		}
		if indent < len(ln) && ln[indent] == '\t' {
			return nil, fmt.Errorf("第 %d 行用 tab 缩进，YAML 不接受", i+1)
		}
		out = append(out, yamlLine{no: i + 1, indent: indent, text: strings.TrimRight(ln[indent:], " ")})
	}
	return out, nil
}

func parseYAMLBlock(lines []yamlLine, i, indent int) (any, int, error) {
	if i >= len(lines) {
		return nil, i, nil
	}
	if lines[i].indent != indent {
		return nil, i, fmt.Errorf("第 %d 行缩进 %d，期望 %d", lines[i].no, lines[i].indent, indent)
	}
	if isYAMLSeqLine(lines[i].text) {
		return parseYAMLSeq(lines, i, indent)
	}
	return parseYAMLMap(lines, i, indent)
}

func isYAMLSeqLine(s string) bool {
	return s == "-" || strings.HasPrefix(s, "- ")
}

func parseYAMLSeq(lines []yamlLine, i, indent int) (any, int, error) {
	out := []any{}
	for i < len(lines) && lines[i].indent == indent && isYAMLSeqLine(lines[i].text) {
		rest := strings.TrimSpace(strings.TrimPrefix(lines[i].text, "-"))
		if rest == "" {
			if i+1 < len(lines) && lines[i+1].indent > indent {
				v, next, err := parseYAMLBlock(lines, i+1, lines[i+1].indent)
				if err != nil {
					return nil, i, err
				}
				out = append(out, v)
				i = next
				continue
			}
			out = append(out, nil)
			i++
			continue
		}
		// "- key: value" 等价于缩进 +2 的 mapping 首行，展平后复用 mapping 解析。
		if _, _, ok := yamlSplitKey(rest); ok {
			lines[i] = yamlLine{no: lines[i].no, indent: indent + 2, text: rest}
			v, next, err := parseYAMLMap(lines, i, indent+2)
			if err != nil {
				return nil, i, err
			}
			out = append(out, v)
			i = next
			continue
		}
		v, err := yamlScalar(rest)
		if err != nil {
			return nil, i, fmt.Errorf("第 %d 行: %w", lines[i].no, err)
		}
		out = append(out, v)
		i++
	}
	return out, i, nil
}

func parseYAMLMap(lines []yamlLine, i, indent int) (any, int, error) {
	out := map[string]any{}
	for i < len(lines) && lines[i].indent == indent {
		key, val, ok := yamlSplitKey(lines[i].text)
		if !ok {
			return nil, i, fmt.Errorf("第 %d 行不是 key: value 形式: %q", lines[i].no, lines[i].text)
		}
		if val == "" {
			// 值在下一层；没有下一层就是 null。
			if i+1 < len(lines) && lines[i+1].indent > indent {
				v, next, err := parseYAMLBlock(lines, i+1, lines[i+1].indent)
				if err != nil {
					return nil, i, err
				}
				out[key] = v
				i = next
				continue
			}
			out[key] = nil
			i++
			continue
		}
		v, err := yamlScalar(val)
		if err != nil {
			return nil, i, fmt.Errorf("第 %d 行 %q: %w", lines[i].no, key, err)
		}
		out[key] = v
		i++
	}
	return out, i, nil
}

// yamlSplitKey 只按第一个 ": " 切。路径键（/v1/runs/{id}）与引号键（"200"）都走这里。
func yamlSplitKey(s string) (key, val string, ok bool) {
	if idx := strings.Index(s, ": "); idx >= 0 {
		return yamlUnquote(strings.TrimSpace(s[:idx])), strings.TrimSpace(s[idx+2:]), true
	}
	if strings.HasSuffix(s, ":") {
		return yamlUnquote(strings.TrimSpace(s[:len(s)-1])), "", true
	}
	return "", "", false
}

func yamlUnquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func yamlScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return nil, nil
	case "[]":
		return []any{}, nil
	case "{}":
		return map[string]any{}, nil
	}
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("流式数组没有闭合: %q", s)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}, nil
		}
		parts := strings.Split(inner, ",")
		arr := make([]any, 0, len(parts))
		for _, p := range parts {
			v, err := yamlScalar(p)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		v, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("双引号字符串无法解析: %q", s)
		}
		return v, nil
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "~":
		return nil, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, nil
	}
	return s, nil
}

// docsHTML 是契约浏览页。
//
// 为什么把页面也嵌进二进制：离线环境（内网、无外网沙箱）也要能看契约。
// 所以不引 CDN、不用构建步骤，一切原生 JS + 内联样式。
// 为什么数据来自 /v1/openapi.json 而不是再渲染一遍 YAML：一个出口、一份数据，
// 页面与抓取端看到的永远是同一份东西。
const docsHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>astra-agent API</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #ffffff; --fg: #1b1f24; --muted: #5b6672; --line: #e3e7ec;
    --panel: #f6f8fa; --accent: #0b6bcb; --code: #f2f4f7;
    --get: #1a7f37; --post: #0b6bcb; --patch: #9a6700; --delete: #b42318;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0d1117; --fg: #e6edf3; --muted: #8b949e; --line: #21262d;
      --panel: #141a21; --accent: #58a6ff; --code: #161b22;
      --get: #3fb950; --post: #58a6ff; --patch: #d29922; --delete: #f85149;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--fg);
    font: 14px/1.6 -apple-system, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif;
    display: grid; grid-template-columns: 300px 1fr; height: 100vh;
  }
  aside { border-right: 1px solid var(--line); overflow-y: auto; padding: 16px 0; background: var(--panel); }
  aside h1 { font-size: 15px; margin: 0 16px 12px; }
  aside h1 small { display: block; color: var(--muted); font-weight: 400; }
  .tag { margin: 14px 16px 4px; font-size: 12px; color: var(--muted); letter-spacing: .04em; text-transform: uppercase; }
  .op {
    display: flex; gap: 8px; align-items: baseline; padding: 4px 16px;
    cursor: pointer; border: 0; background: none; color: inherit;
    width: 100%; text-align: left; font: inherit;
  }
  .op:hover, .op:focus-visible { background: var(--bg); outline: none; }
  .op[aria-current="true"] { background: var(--bg); box-shadow: inset 3px 0 0 var(--accent); }
  .op span.path { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12.5px; word-break: break-all; }
  main { overflow-y: auto; padding: 28px 36px; }
  .method { font-family: ui-monospace, Menlo, monospace; font-size: 11px; font-weight: 700; letter-spacing: .06em; }
  .m-get { color: var(--get); } .m-post { color: var(--post); }
  .m-patch { color: var(--patch); } .m-delete { color: var(--delete); }
  h2 { font-size: 20px; margin: 0 0 6px; font-family: ui-monospace, Menlo, monospace; word-break: break-all; }
  h3 { font-size: 13px; text-transform: uppercase; letter-spacing: .06em; color: var(--muted); margin: 28px 0 10px; }
  p.summary { color: var(--muted); margin: 0 0 4px; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 6px 10px; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { font-size: 12px; color: var(--muted); font-weight: 600; }
  code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12.5px; }
  code { background: var(--code); padding: 1px 5px; border-radius: 4px; }
  pre { background: var(--code); padding: 12px 14px; border-radius: 8px; overflow-x: auto; }
  .req { color: var(--delete); }
  .hint { color: var(--muted); }
  .empty { color: var(--muted); }
</style>
</head>
<body>
<aside>
  <h1>astra-agent API <small id="ver">加载中…</small></h1>
  <div id="nav"></div>
</aside>
<main><p class="empty" id="main">正在读取 /v1/openapi.json …</p></main>
<script>
(function () {
  var METHOD_ORDER = ["get", "post", "patch", "put", "delete"];
  var TAG_ORDER = ["Health", "Auth", "Sessions", "Runs", "Approvals", "Sandboxes", "Usage", "Admin", "LLM", "Meta"];
  var spec = null;
  var ops = [];

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  }

  function deref(node) {
    var seen = 0;
    while (node && node["$ref"] && seen < 20) {
      var ref = node["$ref"];
      if (ref.indexOf("#/") !== 0) return node;
      var cur = spec;
      ref.slice(2).split("/").forEach(function (p) {
        p = p.replace(/~1/g, "/").replace(/~0/g, "~");
        cur = cur ? cur[p] : undefined;
      });
      node = cur;
      seen++;
    }
    return node || {};
  }

  function schemaBrief(node) {
    var s = deref(node);
    if (!s) return "any";
    if (s["$ref"]) return s["$ref"].split("/").pop();
    if (s.type === "array") return "[" + schemaBrief(s.items || {}) + "]";
    if (s.enum) return (s.type || "string") + " (" + s.enum.join(" | ") + ")";
    return s.type || (s.properties ? "object" : "any");
  }

  function renderNav() {
    var nav = document.getElementById("nav");
    nav.textContent = "";
    var groups = {};
    ops.forEach(function (op) {
      (groups[op.tag] = groups[op.tag] || []).push(op);
    });
    var tags = Object.keys(groups).sort(function (a, b) {
      var ia = TAG_ORDER.indexOf(a), ib = TAG_ORDER.indexOf(b);
      if (ia < 0) ia = 99;
      if (ib < 0) ib = 99;
      return ia - ib || a.localeCompare(b);
    });
    tags.forEach(function (tag) {
      nav.appendChild(el("div", "tag", tag));
      groups[tag].forEach(function (op) {
        var b = el("button", "op");
        b.type = "button";
        b.appendChild(el("span", "method m-" + op.method, op.method.toUpperCase()));
        b.appendChild(el("span", "path", op.path));
        b.addEventListener("click", function () { select(op); });
        op.node = b;
        nav.appendChild(b);
      });
    });
  }

  function select(op) {
    ops.forEach(function (o) {
      if (o.node) o.node.setAttribute("aria-current", o === op ? "true" : "false");
    });
    var main = document.getElementById("main");
    main.textContent = "";
    var d = op.def;

    var head = el("div");
    head.appendChild(el("span", "method m-" + op.method, op.method.toUpperCase() + " "));
    head.appendChild(el("span", "hint", op.tag));
    main.appendChild(head);
    main.appendChild(el("h2", null, op.path));
    var summary = d.summary || d.description || "";
    if (summary) main.appendChild(el("p", "summary", summary));
    if (spec.info && spec.info.description && op.first) {
      main.appendChild(el("p", "hint", spec.info.description));
    }
    if (d.deprecated) main.appendChild(el("p", "hint", "该操作已废弃。"));

    var params = (op.pathItem.parameters || []).concat(d.parameters || []).map(deref);
    main.appendChild(el("h3", null, "参数"));
    if (params.length === 0) {
      main.appendChild(el("p", "hint", "无"));
    } else {
      var t = el("table");
      var hr = el("tr");
      ["名称", "位置", "必填", "类型", "说明"].forEach(function (h) { hr.appendChild(el("th", null, h)); });
      t.appendChild(hr);
      params.forEach(function (p) {
        var tr = el("tr");
        tr.appendChild(el("td", null, p.name || ""));
        tr.appendChild(el("td", "hint", p["in"] || ""));
        var reqCell = el("td", p.required ? "req" : "hint", p.required ? "是" : "否");
        tr.appendChild(reqCell);
        tr.appendChild(el("td", null, schemaBrief(p.schema)));
        tr.appendChild(el("td", "hint", p.description || ""));
        t.appendChild(tr);
      });
      main.appendChild(t);
    }

    var body = d.requestBody ? deref(d.requestBody) : null;
    main.appendChild(el("h3", null, "请求体"));
    if (!body) {
      main.appendChild(el("p", "hint", "无"));
    } else {
      var ct = Object.keys(body.content || {});
      ct.forEach(function (k) {
        main.appendChild(el("p", "hint", k + (body.required ? "（必填）" : "")));
        main.appendChild(el("pre", null, JSON.stringify(schemaShape(body.content[k].schema), null, 2)));
      });
    }

    main.appendChild(el("h3", null, "响应"));
    var resp = d.responses || {};
    Object.keys(resp).forEach(function (code) {
      var r = deref(resp[code]);
      main.appendChild(el("p", null, code + " · " + (r.description || "")));
      if (r.content) {
        Object.keys(r.content).forEach(function (k) {
          main.appendChild(el("p", "hint", k));
          main.appendChild(el("pre", null, JSON.stringify(schemaShape(r.content[k].schema), null, 2)));
        });
      }
    });
    main.scrollTop = 0;
    if (history.replaceState) {
      history.replaceState(null, "", "#" + op.method + ":" + op.path);
    }
  }

  // schemaShape 把 schema 折成一个「字段 -> 类型」的摘要，够看即可，不做完整展开。
  function schemaShape(node, depth) {
    depth = depth || 0;
    var s = deref(node);
    if (!s || depth > 4) return "…";
    if (s.type === "array" || s.items) return [schemaShape(s.items || {}, depth + 1)];
    if (s.properties) {
      var out = {};
      Object.keys(s.properties).forEach(function (k) {
        var p = deref(s.properties[k]);
        var v = p.enum ? schemaBrief(p) : (p.properties || p.items ? schemaShape(p, depth + 1) : (p.type || "any"));
        if (p.format) v = String(v) + " (" + p.format + ")";
        if (p.description) v = String(v) + " — " + p.description;
        out[k] = v;
      });
      return out;
    }
    if (s.additionalProperties) return "map<string, " + schemaBrief(s.additionalProperties) + ">";
    return schemaBrief(s);
  }

  fetch("/v1/openapi.json", { headers: { "Accept": "application/json" } })
    .then(function (r) {
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    })
    .then(function (doc) {
      spec = doc;
      document.getElementById("ver").textContent = "v" + ((doc.info && doc.info.version) || "?");
      document.title = ((doc.info && doc.info.title) || "API") + " 契约";
      var paths = doc.paths || {};
      Object.keys(paths).forEach(function (p) {
        METHOD_ORDER.forEach(function (m) {
          var d = paths[p][m];
          if (!d) return;
          ops.push({
            method: m,
            path: p,
            def: d,
            pathItem: paths[p],
            tag: (d.tags && d.tags[0]) || "其它",
            first: ops.length === 0
          });
        });
      });
      renderNav();
      var want = decodeURIComponent((location.hash || "").replace(/^#/, ""));
      var hit = null;
      ops.forEach(function (o) { if (o.method + ":" + o.path === want) hit = o; });
      select(hit || ops[0]);
    })
    .catch(function (e) {
      document.getElementById("main").textContent = "读取契约失败：" + e.message;
    });
})();
</script>
</body>
</html>
`
